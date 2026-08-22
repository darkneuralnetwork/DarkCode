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

// TestRefusingTheSameCallIsRememberedOnce. Repeating an identical call must
// not re-ask; the user already answered that exact question.
func TestRefusingTheSameCallIsRememberedOnce(t *testing.T) {
	var asked int32
	refuse := func(req ApprovalRequest) Verdict {
		atomic.AddInt32(&asked, 1)
		return Verdict{Decision: DecisionDeny}
	}
	g := gateWithApprover(t, refuse, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _, _ := g.Check("terminal", map[string]interface{}{"command": "rm -rf /"}); ok {
			t.Fatal("a refused call was allowed")
		}
	}
	if n := atomic.LoadInt32(&asked); n != 1 {
		t.Errorf("the approver was consulted %d times for the identical call", n)
	}
}

// TestRefusalDoesNotBlockTheWholeTool. Saying no to writing /etc/passwd is not
// saying no to writing anything. Keyed on the tool name alone, one refusal
// disabled the tool for the rest of the session with no way back.
func TestRefusalDoesNotBlockTheWholeTool(t *testing.T) {
	var seen []string
	approver := func(req ApprovalRequest) Verdict {
		path, _ := req.Args["path"].(string)
		seen = append(seen, path)
		if path == "/etc/passwd" {
			return Verdict{Decision: DecisionDeny}
		}
		return Verdict{Decision: DecisionAllowOnce}
	}
	g := gateWithApprover(t, approver, time.Minute)

	if ok, _, _ := g.Check("write_file", map[string]interface{}{"path": "/etc/passwd"}); ok {
		t.Fatal("the refused path was allowed")
	}
	if ok, _, _ := g.Check("write_file", map[string]interface{}{"path": "main.go"}); !ok {
		t.Error("a different path was refused because an earlier one was; the " +
			"refusal was applied to the whole tool")
	}
	if len(seen) != 2 {
		t.Errorf("the approver saw %v; the second call should have been asked", seen)
	}
}

// TestArgumentOrderDoesNotChangeTheKey. Go map iteration is random, so a key
// built from unsorted arguments would differ between two identical calls and
// re-prompt for a question already answered.
func TestArgumentOrderDoesNotChangeTheKey(t *testing.T) {
	a := map[string]interface{}{"path": "x", "content": "y", "mode": 644}
	b := map[string]interface{}{"mode": 644, "content": "y", "path": "x"}
	for i := 0; i < 50; i++ {
		if callKey("write_file", a) != callKey("write_file", b) {
			t.Fatal("the same call produced two different keys")
		}
	}
	if callKey("write_file", a) == callKey("write_file", map[string]interface{}{"path": "z"}) {
		t.Error("different arguments produced the same key")
	}
	if callKey("a", map[string]interface{}{"x": "1"}) == callKey("a", map[string]interface{}{"x1": ""}) {
		t.Error("the separator does not stop key and value running together")
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
