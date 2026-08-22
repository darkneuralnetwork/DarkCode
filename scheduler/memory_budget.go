package scheduler

import (
	"fmt"
	"sync"
)

// MemoryBudget tracks the RAM/VRAM allocations.
type MemoryBudget struct {
	mu          sync.Mutex
	Total       int64 // Total bytes allowed for the entire application
	ModelSlot   int64 // Reserved for local models
	IndexSlot   int64 // Reserved for AST indices
	ContextSlot int64 // Reserved for context window processing
	Used        int64 // Currently allocated
}

// NewMemoryBudget initializes memory budgets based on total available RAM.
func NewMemoryBudget(total int64) *MemoryBudget {
	return &MemoryBudget{
		Total:       total,
		ModelSlot:   int64(float64(total) * 0.4), // 40% for models
		IndexSlot:   int64(float64(total) * 0.2), // 20% for indexing
		ContextSlot: int64(float64(total) * 0.1), // 10% for context processing
		Used:        0,
	}
}

// Allocate requests memory and returns an error if it exceeds the budget.
func (mb *MemoryBudget) Allocate(amount int64) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.Used+amount > mb.Total {
		return fmt.Errorf("out of memory budget: requested %d, free %d", amount, mb.Total-mb.Used)
	}
	mb.Used += amount
	return nil
}

// Free releases memory back to the budget.
func (mb *MemoryBudget) Free(amount int64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.Used -= amount
	if mb.Used < 0 {
		mb.Used = 0
	}
}
