package tools

// ============================================================================
// CHANGE RECORDER
//
// A thread-safe log of every mutating action taken by a tool. The registry
// records a Change before/after each dangerous tool runs (file writes, patches,
// shell commands, git mutations). The CLI console reads this to show the user
// exactly which files were modified and what the previous vs. new content was,
// while keeping stdout free of intermediate logs.
// ============================================================================

import (
	"sync"
	"time"

	"github.com/darkcode/core"
)

// ChangeRecorder collects Change records for a session.
type ChangeRecorder struct {
	mu      sync.Mutex
	changes []core.Change
}

// NewChangeRecorder creates an empty recorder.
func NewChangeRecorder() *ChangeRecorder {
	return &ChangeRecorder{changes: make([]core.Change, 0)}
}

// Record appends a change.
func (r *ChangeRecorder) Record(c core.Change) {
	if r == nil {
		return
	}
	if c.Timestamp.IsZero() {
		c.Timestamp = time.Now()
	}
	r.mu.Lock()
	r.changes = append(r.changes, c)
	r.mu.Unlock()
}

// All returns a copy of every recorded change.
func (r *ChangeRecorder) All() []core.Change {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]core.Change, len(r.changes))
	copy(out, r.changes)
	return out
}

// Since returns changes recorded after the given index (exclusive).
// If idx is negative it is treated as 0.
func (r *ChangeRecorder) Since(idx int) []core.Change {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 {
		idx = 0
	}
	if idx > len(r.changes) {
		idx = len(r.changes)
	}
	out := make([]core.Change, len(r.changes)-idx)
	copy(out, r.changes[idx:])
	return out
}

// Len returns the number of recorded changes.
func (r *ChangeRecorder) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.changes)
}

// Clear empties the recorder.
func (r *ChangeRecorder) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.changes = r.changes[:0]
	r.mu.Unlock()
}
