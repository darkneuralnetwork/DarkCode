package scheduler

import (
	"fmt"
	"sync"
)

// ContextBudget manages token allocations per request to prevent context overflow.
type ContextBudget struct {
	mu          sync.Mutex
	MaxTokens   int
	UsedTokens  int
}

func NewContextBudget() *ContextBudget {
	return &ContextBudget{
		MaxTokens: 128000, // Default for testing, should be dynamic based on model
	}
}

// Check capacity
func (cb *ContextBudget) Check(tokens int) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.UsedTokens + tokens > cb.MaxTokens {
		return fmt.Errorf("context budget exceeded")
	}
	return nil
}
