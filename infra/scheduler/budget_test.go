package scheduler

import "testing"

func TestMemoryBudgetAllocateFree(t *testing.T) {
	mb := NewMemoryBudget(1000)

	if err := mb.Allocate(600); err != nil {
		t.Fatalf("first allocation within budget failed: %v", err)
	}
	if err := mb.Allocate(600); err == nil {
		t.Error("allocation exceeding the budget should error")
	}
	if mb.Used != 600 {
		t.Errorf("a rejected allocation must not change Used: got %d, want 600", mb.Used)
	}

	mb.Free(600)
	if mb.Used != 0 {
		t.Errorf("Used = %d after freeing all, want 0", mb.Used)
	}
	// Over-free must clamp at zero, not go negative.
	mb.Free(500)
	if mb.Used != 0 {
		t.Errorf("over-free should clamp to 0, got %d", mb.Used)
	}
}

func TestContextBudgetCheck(t *testing.T) {
	cb := NewContextBudget()
	cb.MaxTokens = 100
	if err := cb.Check(100); err != nil {
		t.Errorf("exact-fit check should pass: %v", err)
	}
	if err := cb.Check(101); err == nil {
		t.Error("check exceeding MaxTokens should error")
	}
}
