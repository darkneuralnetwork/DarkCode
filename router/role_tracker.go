package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// RoleWeight tracks per-model, per-role performance for weighted synthesis.
type RoleWeight struct {
	Role         string  `json:"role"`
	ModelName    string  `json:"model"`
	TotalCalls   int     `json:"total_calls"`
	SuccessRate  float64 `json:"success_rate"`
	AvgLatencyMs int64   `json:"avg_latency_ms,omitempty"`
	ConflictRate float64 `json:"conflict_rate,omitempty"`
	Weight       float64 `json:"weight"`
}

// RoleTracker records how reliably each model performs each kind of work, and
// feeds that back into consensus weighting and model selection.
//
// It persists. An in-memory tracker relearns the same lesson after every
// restart, which is the difference between a system that improves with use and
// one that only appears to.
type RoleTracker struct {
	mu      sync.RWMutex
	weights map[string]map[string]*RoleWeight // role/task kind → model → weight
	path    string
}

func NewRoleTracker() *RoleTracker {
	return &RoleTracker{weights: make(map[string]map[string]*RoleWeight)}
}

// SetPersistPath enables persistence and loads any prior record. A failure to
// read is not fatal: the tracker simply starts empty.
func (rt *RoleTracker) SetPersistPath(path string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.path = path

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var flat []RoleWeight
	if json.Unmarshal(data, &flat) != nil {
		return
	}
	for i := range flat {
		w := flat[i]
		if rt.weights[w.Role] == nil {
			rt.weights[w.Role] = map[string]*RoleWeight{}
		}
		rt.weights[w.Role][w.ModelName] = &w
	}
}

// saveLocked writes the record. Called with the lock held.
func (rt *RoleTracker) saveLocked() {
	if rt.path == "" {
		return
	}
	var flat []RoleWeight
	for _, models := range rt.weights {
		for _, w := range models {
			flat = append(flat, *w)
		}
	}
	sort.Slice(flat, func(i, j int) bool {
		if flat[i].Role != flat[j].Role {
			return flat[i].Role < flat[j].Role
		}
		return flat[i].ModelName < flat[j].ModelName
	})
	data, err := json.MarshalIndent(flat, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(rt.path), 0o755); err != nil {
		return
	}
	tmp := rt.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, rt.path)
	}
}

func (rt *RoleTracker) GetWeight(role, modelName string) float64 {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rm, ok := rt.weights[role]; ok {
		if rw, ok := rm[modelName]; ok {
			return rw.Weight
		}
	}
	return 1.0 // Default weight
}

// minCallsForDemotion is how much evidence is needed before a model's record
// is allowed to count against it. Below this, one unlucky failure would
// permanently sideline a perfectly good model.
const minCallsForDemotion = 5

// unreliableFloor is the success rate below which a model is considered to be
// failing this kind of work.
const unreliableFloor = 0.35

// Unreliable reports whether a model has demonstrably and repeatedly failed at
// this role, so callers can prefer another one. Unknown models are never
// unreliable — a model has to earn a bad reputation.
func (rt *RoleTracker) Unreliable(role, modelName string) bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	rm, ok := rt.weights[role]
	if !ok {
		return false
	}
	rw, ok := rm[modelName]
	return ok && rw.TotalCalls >= minCallsForDemotion && rw.SuccessRate < unreliableFloor
}

// Stats returns a snapshot of every tracked model/role pair, for reporting.
func (rt *RoleTracker) Stats() []RoleWeight {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	var out []RoleWeight
	for _, models := range rt.weights {
		for _, w := range models {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SuccessRate > out[j].SuccessRate })
	return out
}

func (rt *RoleTracker) RecordSuccess(role, modelName string, success bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if _, ok := rt.weights[role]; !ok {
		rt.weights[role] = make(map[string]*RoleWeight)
	}
	if _, ok := rt.weights[role][modelName]; !ok {
		rt.weights[role][modelName] = &RoleWeight{
			Role:      role,
			ModelName: modelName,
			Weight:    1.0,
			// Start optimistic so the first observation moves the rate
			// meaningfully instead of crawling up from zero.
			SuccessRate: 1.0,
		}
	}

	rw := rt.weights[role][modelName]
	rw.TotalCalls++

	var sVal float64
	if success {
		sVal = 1.0
	}
	// Moving average for success rate
	rw.SuccessRate = rw.SuccessRate*0.9 + sVal*0.1
	// Recompute weight
	rw.Weight = 0.5 + (rw.SuccessRate * 1.5)

	rt.saveLocked()
}
