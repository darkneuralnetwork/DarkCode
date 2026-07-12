package memory

// layers.go — ArchitectureMemory, a persisted key/value store for codebase
// design decisions (component map, dependency notes, architectural
// constraints) recorded by the agent across a session.
//
// This used to be one of six "Phase 9" memory tiers (Conversation, Session,
// Workspace, Project, Architecture, User). An audit found the other five
// fully redundant with existing, already-wired systems — Conversation with
// System.stm, Project with project.Store, Session/Workspace with no real
// caller and dedicated typed fields where needed, User with the existing
// semantic-tier "user" category in tools/memory_tool.go — so they were
// removed rather than left as unused dead weight. ArchitectureMemory was the
// one tier with no existing equivalent (the knowledge graph captures task/
// tool/concept activity, not "why we designed it this way" decisions), so it
// was kept and wired into System with disk persistence — see
// System.ArchitectureAddDecision / System.ArchitectureDecisions.

import "sync"

// kvMemory is the shared map-backed implementation ArchitectureMemory builds on.
type kvMemory struct {
	mu   sync.RWMutex
	data map[string]interface{}
}

func newKV() kvMemory { return kvMemory{data: make(map[string]interface{})} }

// Set stores a value under key.
func (m *kvMemory) Set(key string, value interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
}

// Get retrieves a value by key. Returns nil, false if not found.
func (m *kvMemory) Get(key string) (interface{}, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	return v, ok
}

// Delete removes a key.
func (m *kvMemory) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
}

// Keys returns all keys (sorted by the caller if needed).
func (m *kvMemory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out
}

// Clear removes all entries.
func (m *kvMemory) Clear() {
	m.mu.Lock()
	m.data = make(map[string]interface{})
	m.mu.Unlock()
}

// Size returns the number of stored keys.
func (m *kvMemory) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// ---------------------------------------------------------------------------
// ArchitectureMemory
// ---------------------------------------------------------------------------

// ArchitectureMemory tracks the codebase's high-level design: component map,
// dependency graph, design decisions, architectural constraints. Persisted to
// disk by System (architecture.json) — see System.ArchitectureAddDecision.
type ArchitectureMemory struct {
	kvMemory
}

func NewArchitectureMemory() *ArchitectureMemory {
	return &ArchitectureMemory{kvMemory: newKV()}
}

// AddDecision records an architectural decision (convenience method). Callers
// that want the decision persisted to disk should go through
// System.ArchitectureAddDecision instead of calling this directly, since
// System owns the debounced writer for this store.
func (m *ArchitectureMemory) AddDecision(title, decision string) {
	m.Set("decision:"+title, decision)
}

// Decisions returns all recorded architectural decisions.
func (m *ArchitectureMemory) Decisions() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string)
	for k, v := range m.data {
		if len(k) > 9 && k[:9] == "decision:" {
			if s, ok := v.(string); ok {
				out[k[9:]] = s
			}
		}
	}
	return out
}
