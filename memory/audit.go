package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// ============================================================================
// AUDIT LOG — Append-only action trail for governance and compliance
//
// Every significant action (tool use, agent spawn, safety check, approval)
// is recorded in the audit log. Actions are classified by risk level:
//   Low      — automatic execution, just logged
//   Medium   — logged with detail
//   High     — requires human approval
//   Critical — blocked by default
// ============================================================================

// AuditLog is an append-only audit trail.
type AuditLog struct {
	mu       sync.RWMutex
	entries  []core.AuditEntry
	filePath string
	counter  int64
	writer   *DebouncedWriter
}

// NewAuditLog creates an audit log with persistence.
func NewAuditLog(dir string) (*AuditLog, error) {
	path := filepath.Join(dir, "audit_log.json")
	al := &AuditLog{
		filePath: path,
	}
	if err := al.load(); err != nil {
		return nil, err
	}
	al.counter = int64(len(al.entries))

	al.writer = NewDebouncedWriter(path, 2*time.Second, func() ([]byte, error) {
		al.mu.RLock()
		defer al.mu.RUnlock()
		return json.Marshal(al.entries)
	})

	return al, nil
}

// Shutdown flushes pending writes to disk.
func (al *AuditLog) Shutdown() {
	if al.writer != nil {
		al.writer.Shutdown()
	}
}

// Record adds an entry to the audit log.
func (al *AuditLog) Record(entry core.AuditEntry) error {
	al.mu.Lock()
	defer al.mu.Unlock()

	al.counter++
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("audit_%d_%d", time.Now().Unix(), al.counter)
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	al.entries = append(al.entries, entry)
	al.writer.MarkDirty()
	return nil
}

// RecordAction is a convenience method for common audit entries.
func (al *AuditLog) RecordAction(agent core.AgentRole, action, tool string, risk core.RiskLevel, approved bool, outcome string) error {
	approvedBy := "auto"
	if risk == core.RiskHigh || risk == core.RiskCritical {
		if approved {
			approvedBy = "human"
		} else {
			approvedBy = "denied"
		}
	} else {
		approvedBy = "policy"
	}

	return al.Record(core.AuditEntry{
		Agent:      agent,
		Action:     action,
		Tool:       tool,
		RiskLevel:  risk,
		Approved:   approved,
		ApprovedBy: approvedBy,
		Outcome:    outcome,
	})
}

// GetAll returns all audit entries, most recent first.
func (al *AuditLog) GetAll() []core.AuditEntry {
	al.mu.RLock()
	defer al.mu.RUnlock()
	result := make([]core.AuditEntry, len(al.entries))
	copy(result, al.entries)
	// Reverse for most-recent-first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// GetRecent returns the N most recent entries.
func (al *AuditLog) GetRecent(n int) []core.AuditEntry {
	all := al.GetAll()
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// GetByRisk returns entries at or above the given risk level.
func (al *AuditLog) GetByRisk(minRisk core.RiskLevel) []core.AuditEntry {
	al.mu.RLock()
	defer al.mu.RUnlock()

	riskOrder := map[core.RiskLevel]int{
		core.RiskLow:      0,
		core.RiskMedium:   1,
		core.RiskHigh:     2,
		core.RiskCritical: 3,
	}

	minLevel := riskOrder[minRisk]
	var result []core.AuditEntry
	for _, entry := range al.entries {
		if riskOrder[entry.RiskLevel] >= minLevel {
			result = append(result, entry)
		}
	}
	return result
}

// Count returns the total number of audit entries.
func (al *AuditLog) Count() int {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return len(al.entries)
}

// Summary returns audit statistics.
func (al *AuditLog) Summary() map[string]interface{} {
	al.mu.RLock()
	defer al.mu.RUnlock()

	riskCounts := make(map[string]int)
	approvalCounts := make(map[string]int)
	agentCounts := make(map[string]int)
	denied := 0

	for _, e := range al.entries {
		riskCounts[string(e.RiskLevel)]++
		approvalCounts[e.ApprovedBy]++
		agentCounts[string(e.Agent)]++
		if !e.Approved {
			denied++
		}
	}

	return map[string]interface{}{
		"total":           len(al.entries),
		"denied":          denied,
		"risk_breakdown":  riskCounts,
		"approval_types":  approvalCounts,
		"agent_breakdown": agentCounts,
	}
}

// ============================================================================
// PERSISTENCE
// ============================================================================

func (al *AuditLog) load() error {
	data, err := os.ReadFile(al.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &al.entries)
}
