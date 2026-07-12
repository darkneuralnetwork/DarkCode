package router

import "sync"

// RoleWeight tracks per-model, per-role performance for weighted synthesis.
type RoleWeight struct {
	Role         string
	ModelName    string
	TotalCalls   int
	SuccessRate  float64
	AvgLatencyMs int64
	ConflictRate float64
	Weight       float64
}

// RoleTracker tracks metrics for adaptive consensus.
type RoleTracker struct {
	mu      sync.RWMutex
	weights map[string]map[string]*RoleWeight // role -> modelName -> weight
}

func NewRoleTracker() *RoleTracker {
	return &RoleTracker{
		weights: make(map[string]map[string]*RoleWeight),
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
}
