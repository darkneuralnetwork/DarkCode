package permission

import (
	"sync/atomic"
	"testing"
	"time"
)

// A denial the user actually gave sticks for the session. A prompt that timed
// out is the absence of an answer — caching it silently killed the tool for
// the rest of the run, so stepping away from one prompt looked like the agent
// losing the ability to write files.

func gateWithApprover(t *testing.T, a Approver, timeout time.Duration) *Gate {
	t.Helper()
	g := NewGate(LevelStrict)
	g.SetApprover(a)
	g.SetAskTimeout(timeout)
	return g
}

// TestTimedOutPromptDeniesButAsksAgain is the fix: the call fails closed, and
// the next one still reaches the user.
func TestTimedOutPromptDeniesButAsksAgain(t *testing.T) {
	var asked int32
	slow := func(req ApprovalRequest) Verdict {
		atomic.AddInt32(&asked, 1)
		time.Sleep(300 * time.Millisecond) // outlives the timeout below
		return Verdict{Decision: DecisionAllowOnce}
	}
	g := gateWithApprover(t, slow, 30*time.Millisecond)

	if ok, _, _ := g.Check("write_file", map[string]interface{}{"path": "a.go"}); ok {
		t.Error("a timed-out approval was allowed; it must fail closed")
	}
	if ok, _, _ := g.Check("write_file", map[string]interface{}{"path": "b.go"}); ok {
		t.Error("second call allowed unexpectedly")
	}

	if n := atomic.LoadInt32(&asked); n < 2 {
		t.Errorf("the approver was consulted %d time(s); after a timeout the "+
			"next call must still ask rather than be silently refused", n)
	}
}

// TestExplicitDenyStillSticks. The user saying no is a decision, and repeating
// the prompt for every later call to the same tool is its own problem.
func TestExplicitDenyStillSticks(t *testing.T) {
	var asked int32
	refuse := func(req ApprovalRequest) Verdict {
		atomic.AddInt32(&asked, 1)
		return Verdict{Decision: DecisionDeny}
	}
	g := gateWithApprover(t, refuse, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _, _ := g.Check("terminal", map[string]interface{}{"command": "rm -rf /"}); ok {
			t.Fatal("a refused tool was allowed")
		}
	}
	if n := atomic.LoadInt32(&asked); n != 1 {
		t.Errorf("the approver was consulted %d times; an explicit refusal should "+
			"be remembered for the session", n)
	}
}

// TestAllowForSessionStopsPrompting — the other half of the same cache.
func TestAllowForSessionStopsPrompting(t *testing.T) {
	var asked int32
	allow := func(req ApprovalRequest) Verdict {
		atomic.AddInt32(&asked, 1)
		return Verdict{Decision: DecisionAllowSession}
	}
	g := gateWithApprover(t, allow, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _, _ := g.Check("read_file", map[string]interface{}{"path": "a.go"}); !ok {
			t.Fatal("a session-approved tool was refused")
		}
	}
	if n := atomic.LoadInt32(&asked); n != 1 {
		t.Errorf("the approver was consulted %d times despite an allow-for-session", n)
	}
}

// TestAllowOnceKeepsAsking. "Just this once" must not become "always".
func TestAllowOnceKeepsAsking(t *testing.T) {
	var asked int32
	once := func(req ApprovalRequest) Verdict {
		atomic.AddInt32(&asked, 1)
		return Verdict{Decision: DecisionAllowOnce}
	}
	g := gateWithApprover(t, once, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _, _ := g.Check("write_file", map[string]interface{}{"path": "a.go"}); !ok {
			t.Fatal("an allow-once call was refused")
		}
	}
	if n := atomic.LoadInt32(&asked); n != 3 {
		t.Errorf("the approver was consulted %d times; allow-once must ask every time", n)
	}
}

// TestResetSessionClearsBothCaches. A fresh session should not inherit the
// previous one's answers.
func TestResetSessionClearsBothCaches(t *testing.T) {
	var asked int32
	refuse := func(req ApprovalRequest) Verdict {
		atomic.AddInt32(&asked, 1)
		return Verdict{Decision: DecisionDeny}
	}
	g := gateWithApprover(t, refuse, time.Minute)

	g.Check("terminal", map[string]interface{}{"command": "ls"})
	g.ResetSession()
	g.Check("terminal", map[string]interface{}{"command": "ls"})

	if n := atomic.LoadInt32(&asked); n != 2 {
		t.Errorf("the approver was consulted %d times; ResetSession must clear the refusal", n)
	}
}
