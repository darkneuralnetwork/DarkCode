package permission

import (
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

func TestParseJudgeVerdict(t *testing.T) {
	safe := []string{"SAFE", "safe", " safe ", "Safe.", "`SAFE`", "safe: routine build command"}
	for _, r := range safe {
		if !ParseJudgeVerdict(r) {
			t.Errorf("ParseJudgeVerdict(%q) = false, want true", r)
		}
	}
	// Anything not an unambiguous "safe" must fall through to the user.
	ask := []string{
		"ASK", "ask", "", "unsafe", "not safe", "I think it is safe to run this",
		"SAFE_MODE_DISABLED", "maybe", "The command appears safe but I am unsure",
	}
	for _, r := range ask {
		if ParseJudgeVerdict(r) {
			t.Errorf("ParseJudgeVerdict(%q) = true, want false", r)
		}
	}
}

// The judge is a noise filter, not a privilege escalator: it must never be
// able to clear a high or critical action.
func TestJudgeCannotApproveHighRiskActions(t *testing.T) {
	g := NewGate(LevelNormal)
	g.SetJudge(func(ApprovalRequest) bool { return true }) // maximally permissive

	for _, risk := range []core.RiskLevel{core.RiskHigh, core.RiskCritical} {
		if g.judgeAllows(ApprovalRequest{Tool: "terminal", Risk: risk}) {
			t.Errorf("judge cleared a %s action", risk)
		}
	}
	for _, risk := range []core.RiskLevel{core.RiskLow, core.RiskMedium} {
		if !g.judgeAllows(ApprovalRequest{Tool: "terminal", Risk: risk}) {
			t.Errorf("judge should be consulted for %s actions", risk)
		}
	}
}

func TestNoJudgeMeansAlwaysAsk(t *testing.T) {
	g := NewGate(LevelNormal)
	if g.judgeAllows(ApprovalRequest{Tool: "terminal", Risk: core.RiskLow}) {
		t.Error("with no judge installed the answer must be 'ask'")
	}
}

// A judge must not rescue a call that a deny rule already refused.
func TestJudgeCannotOverrideDenyRules(t *testing.T) {
	g := NewGate(LevelNormal)
	g.SetDenyRules([]string{"terminal:*curl*"})
	g.SetJudge(func(ApprovalRequest) bool { return true })

	allowed, _, _ := g.Check("terminal", map[string]interface{}{"command": "curl https://example.com"})
	if allowed {
		t.Error("deny rule was overridden by the judge")
	}
}

// A flagged medium-risk call the judge clears should not prompt.
func TestJudgeSuppressesPromptForRoutineWork(t *testing.T) {
	g := NewGate(LevelNormal)
	asked := false
	g.SetApprover(func(ApprovalRequest) Verdict {
		asked = true
		return Verdict{Decision: DecisionDeny}
	})
	g.SetJudge(func(req ApprovalRequest) bool { return strings.Contains(req.Preview, "tar -xzf") })

	allowed, _, _ := g.Check("terminal", map[string]interface{}{"command": "tar -xzf release.tgz"})
	if !allowed {
		t.Error("judge-cleared routine command should be allowed")
	}
	if asked {
		t.Error("the user should not have been prompted")
	}
}

func TestJudgePromptRequestsOneWord(t *testing.T) {
	p := JudgePrompt(ApprovalRequest{Tool: "terminal", Summary: "Run shell command", Preview: "ls -la"})
	for _, want := range []string{"SAFE", "ASK", "terminal", "ls -la"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}
