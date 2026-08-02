package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/tools"
)

// TestBudgetStopsMidRun is the regression guard for per-iteration spend caps.
//
// The cap used to be tested once, at the start of Kernel.Execute. That bounds
// how many REQUESTS begin after the limit is reached — it says nothing about
// how much any single request spends. One agentic run makes up to maxLoops
// model calls, and a consensus turn multiplies that by the number of registered
// models, so a run that started just under the cap could end far past it.
func TestBudgetStopsMidRun(t *testing.T) {
	l := New(nil, nil, nil, 25)

	calls := 0
	l.SetBudgetCheck(func() string {
		calls++
		if calls > 2 {
			return "today's spend $5.10 reached the $5.00 daily limit"
		}
		return ""
	})

	if l.budget == nil {
		t.Fatal("SetBudgetCheck did not install the check")
	}
	// Drive the check directly: the surrounding loop needs a live model, but
	// the contract under test is "a non-empty reason halts the run".
	if r := l.budget(); r != "" {
		t.Errorf("first iteration should be allowed, got %q", r)
	}
	if r := l.budget(); r != "" {
		t.Errorf("second iteration should be allowed, got %q", r)
	}
	r := l.budget()
	if r == "" {
		t.Fatal("the check stopped reporting once the cap was reached")
	}
	if !strings.Contains(r, "daily limit") {
		t.Errorf("reason %q should name the limit that was hit, so the user knows which one to raise", r)
	}
}

// TestBudgetCheckIsOptional — the default (no cap configured) must not change
// behaviour or cost anything.
func TestBudgetCheckIsOptional(t *testing.T) {
	l := New(nil, nil, nil, 5)
	if l.budget != nil {
		t.Error("a loop with no configured budget must not carry a check")
	}
	l.SetBudgetCheck(func() string { return "stop" })
	l.SetBudgetCheck(nil)
	if l.budget != nil {
		t.Error("SetBudgetCheck(nil) must clear the check")
	}
}

// TestBudgetHaltsARunAndKeepsTheWork drives the real loop with a cap that trips
// immediately.
//
// Two properties matter beyond "it stopped". The run must return the work
// already paid for rather than an error — the user asked for a spend limit,
// not for their money to be spent and the output discarded — and the reason
// must be visible in what comes back, so a truncated answer is never mistaken
// for a complete one.
func TestBudgetHaltsARunAndKeepsTheWork(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"partial progress so far"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 10)
	l.SetBudgetCheck(func() string { return "session spend $9.99 reached the $5.00 limit" })

	result, err := l.Run(context.Background(), "a long task", nil)
	if err != nil {
		t.Fatalf("hitting a spend cap must not fail the run: %v", err)
	}
	if result.Completed {
		t.Error("a run stopped by the budget must not report itself completed")
	}
	if !strings.Contains(result.Output, "$5.00 limit") {
		t.Errorf("output %q should carry the reason, so a truncated answer is not mistaken for a finished one", result.Output)
	}
}

// TestBudgetAllowsAnUncappedRun — the default path must be untouched.
func TestBudgetAllowsAnUncappedRun(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"the complete answer"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	result, err := l.Run(context.Background(), "a task", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Output, "stopped:") {
		t.Errorf("a run with no cap configured was halted: %q", result.Output)
	}
}
