package loop

import (
	"errors"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/router"
)

// The spend cap used to be checked once, before the request started. A single
// request then makes up to twenty-five acting turns plus planning, consensus
// fan-out and sub-agent calls — so a limit was checked once and exceeded
// several times over inside the run it was meant to bound.

// TestBudgetCheckStopsTheLoop. The cap is reached partway through; the
// remaining iterations must not run.
func TestBudgetCheckStopsTheLoop(t *testing.T) {
	l := New(nil, nil, nil, 25)

	calls := 0
	l.SetBudgetCheck(func() error {
		calls++
		if calls > 3 {
			return errors.New("cost limit reached: today's spend $5.01 reached the $5.00 daily limit")
		}
		return nil
	})

	if l.budget == nil {
		t.Fatal("SetBudgetCheck did not install the check")
	}
	// Drive the installed check directly: three clear passes, then a refusal
	// that must persist rather than flapping.
	for i := 0; i < 3; i++ {
		if err := l.budget(); err != nil {
			t.Fatalf("check %d refused early: %v", i+1, err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := l.budget(); err == nil {
			t.Fatal("the check stopped refusing after the cap was reached")
		}
	}
}

// TestBudgetCheckIsOptional. Most installs configure no cap, and the loop must
// behave exactly as before when none is set.
func TestBudgetCheckIsOptional(t *testing.T) {
	l := New(nil, nil, nil, 5)
	if l.budget != nil {
		t.Error("a loop with no cap configured has a budget check")
	}
	l.SetBudgetCheck(func() error { return errors.New("x") })
	l.SetBudgetCheck(nil)
	if l.budget != nil {
		t.Error("passing nil did not clear the check")
	}
}

// TestBudgetErrorReachesTheUser. "The run stopped" without saying why reads as
// the agent giving up, and sends the user to change the wrong setting.
func TestBudgetErrorReachesTheUser(t *testing.T) {
	l := New(nil, nil, nil, 5)
	want := "today's spend $5.01 reached the $5.00 daily limit"
	l.SetBudgetCheck(func() error { return errors.New("cost limit reached: " + want) })

	err := l.budget()
	if err == nil {
		t.Fatal("no error returned")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the reason was lost: %q", err.Error())
	}
}

// codingTierRouter registers a single client under ModelTierCoding with the
// given context window — the tier historyBudgetTokens routes to
// (PurposeExecute's tier), so this is enough to exercise per-model sizing
// without pulling in the full newTestRouter multi-tier setup.
func codingTierRouter(t *testing.T, contextWindow int) *router.Router {
	t.Helper()
	r := router.NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeLLMClient{name: "fake", contextWindow: contextWindow}, "fake-model")
	r.MarkPrimary("fake-model")
	return r
}

// TestHistoryBudgetTokensSizedPerModel is the regression test for the fixed,
// init-time-computed loopHistoryBudgetTokens this replaced: a 32K-context
// model and a 200K-context model used to get the identical history budget
// (both sized against ctxfit.DefaultContextWindow, 128K) because the budget
// was computed once at package init, before any model was ever routed to.
// historyBudgetTokens computes it per call against whatever the loop will
// actually route to, so the two must now differ.
func TestHistoryBudgetTokensSizedPerModel(t *testing.T) {
	small := historyBudgetTokens(codingTierRouter(t, 32000), 3, "a query")
	large := historyBudgetTokens(codingTierRouter(t, 200000), 3, "a query")

	if small >= large {
		t.Fatalf("expected the 32K-context model's budget (%d) to be smaller than the 200K-context model's (%d) — they were sized identically", small, large)
	}
	if small <= 0 || large <= 0 {
		t.Fatalf("expected positive budgets, got small=%d large=%d", small, large)
	}
}

// TestHistoryBudgetTokensFallsBackWhenRoutingFails ensures a router with no
// model registered degrades to the old fixed default rather than panicking
// or returning a nonsensical (zero/negative) budget.
func TestHistoryBudgetTokensFallsBackWhenRoutingFails(t *testing.T) {
	r := router.NewRouter(core.RouteSingle, nil) // nothing registered
	got := historyBudgetTokens(r, 3, "a query")
	if got != fallbackHistoryBudgetTokens {
		t.Errorf("got %d, want the fallback %d when routing fails", got, fallbackHistoryBudgetTokens)
	}
}
