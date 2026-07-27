package permission

import (
	"testing"
	"time"
)

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*rm -rf /*", "sudo rm -rf /var/log", true},
		{"*rm -rf /*", "rm -rf ./build", false},
		{"*/.ssh/*", "/home/user/.ssh/id_rsa", true},
		{"*/.ssh/*", "/home/user/project/main.go", false},
		{"push", "push", true},
		{"push", "pushd", false},
		{"git ?ush", "git push", true},
		{"*", "anything at all", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestParseDenyRules(t *testing.T) {
	rules := ParseDenyRules([]string{"browser", "terminal:*rm -rf*", "  ", "git : push "})
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3 (blank entries dropped)", len(rules))
	}
	if rules[0].Tool != "browser" || rules[0].Pattern != "" {
		t.Errorf("bare tool rule = %+v", rules[0])
	}
	if rules[2].Tool != "git" || rules[2].Pattern != "push" {
		t.Errorf("whitespace around a rule should be trimmed: %+v", rules[2])
	}
}

// A deny rule must beat every permissive path: the relaxed level, a
// session-wide allow, and an approver that would say yes.
func TestDenyRulesBeatEveryPermissivePath(t *testing.T) {
	for _, level := range []Level{LevelRelaxed, LevelNormal, LevelStrict} {
		g := NewGate(level)
		g.SetApprover(AutoApprover())
		g.SetDenyRules([]string{"terminal:*rm -rf /*"})

		args := map[string]interface{}{"command": "rm -rf /etc"}
		if allowed, _, feedback := g.Check("terminal", args); allowed {
			t.Errorf("level %s: deny rule was bypassed", level)
		} else if feedback == "" {
			t.Errorf("level %s: denial should explain which rule fired", level)
		}

		// A previously granted session-wide allow must not rescue it either.
		g.allowed["terminal"] = true
		if allowed, _, _ := g.Check("terminal", args); allowed {
			t.Errorf("level %s: session allow overrode the deny rule", level)
		}

		// Non-matching commands on the same tool still go through.
		if allowed, _, _ := g.Check("terminal", map[string]interface{}{"command": "ls -la"}); !allowed {
			t.Errorf("level %s: unrelated command was blocked", level)
		}
	}
}

func TestDenyRuleWithoutPatternBlocksWholeTool(t *testing.T) {
	g := NewGate(LevelRelaxed)
	g.SetDenyRules([]string{"web_fetch"})
	if allowed, _, _ := g.Check("web_fetch", map[string]interface{}{"url": "https://example.com"}); allowed {
		t.Error("bare tool deny rule did not block the tool")
	}
	if allowed, _, _ := g.Check("read_file", map[string]interface{}{"path": "a.go"}); !allowed {
		t.Error("deny rule leaked to a different tool")
	}
}

// An approver that never answers must not hang the agent forever.
func TestApprovalFailsClosedOnTimeout(t *testing.T) {
	g := NewGate(LevelStrict)
	g.SetAskTimeout(50 * time.Millisecond)
	blocked := make(chan struct{})
	g.SetApprover(func(req ApprovalRequest) Verdict {
		<-blocked // never resolves during the test
		return Verdict{Decision: DecisionAllowSession}
	})
	defer close(blocked)

	start := time.Now()
	allowed, _, feedback := g.Check("write_file", map[string]interface{}{"path": "x", "content": "y"})
	if allowed {
		t.Error("a timed-out approval must deny (fail closed)")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Check blocked for %v, want it to give up near the 50ms timeout", elapsed)
	}
	if feedback == "" {
		t.Error("timeout denial should explain itself")
	}
}
