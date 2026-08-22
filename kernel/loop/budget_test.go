package loop

import (
	"errors"
	"strings"
	"testing"
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
