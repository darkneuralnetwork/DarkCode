package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/tools/tools"
)

// TestContractFailureKeepsTheLoopWorking is the point of the contract. The
// loop's stop condition used to be "the model emitted no tool calls", softened
// by asking the same model whether it was happy with its own work. Both are
// opinions. A failing acceptance check is evidence, and evidence must outrank
// the model's claim to be finished.
func TestContractFailureKeepsTheLoopWorking(t *testing.T) {
	client := &fakeLLMClient{responses: []string{
		"I have finished the task.", // stop attempt 1 — checks still failing
		"Now I have really finished.",
	}}

	calls := 0
	contract := &Contract{
		Criteria: []string{"go build ./... passes"},
		Verify: func(ctx context.Context) Verdict {
			calls++
			if calls == 1 {
				return Verdict{Passed: false, Checked: 1, Evidence: "main.go:12: undefined: foo"}
			}
			return Verdict{Passed: true, Checked: 1}
		},
	}

	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	res, err := l.RunWithContract(context.Background(), "fix the build", nil, contract)
	if err != nil {
		t.Fatalf("RunWithContract: %v", err)
	}
	if calls < 2 {
		t.Fatalf("contract verified %d time(s); a failing check must force another round", calls)
	}
	if !res.Completed {
		t.Error("Completed = false, want true — the checks passed on the second round")
	}
	if !res.Verdict.Proven() {
		t.Error("Verdict.Proven() = false, want true")
	}
}

// TestFailingEvidenceReachesTheModel: the failing command's own output is the
// correction signal. Paraphrasing it into "please try again" throws away the
// only part the model actually needs — the compiler already said what is wrong.
func TestFailingEvidenceReachesTheModel(t *testing.T) {
	const compilerError = "main.go:12:2: undefined: renderTemplate"

	var sawEvidence bool
	client := &fakeLLMClient{
		responses: []string{"done", "done again"},
		onRequest: func(req *core.CompletionRequest) {
			for _, m := range req.Messages {
				if strings.Contains(m.ContentString(), compilerError) {
					sawEvidence = true
				}
			}
		},
	}

	calls := 0
	contract := &Contract{
		Criteria: []string{"go build ./... passes"},
		Verify: func(ctx context.Context) Verdict {
			calls++
			if calls == 1 {
				return Verdict{Passed: false, Checked: 1, Evidence: compilerError}
			}
			return Verdict{Passed: true, Checked: 1}
		},
	}

	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	if _, err := l.RunWithContract(context.Background(), "build the page", nil, contract); err != nil {
		t.Fatalf("RunWithContract: %v", err)
	}
	if !sawEvidence {
		t.Error("the failing command's output never reached the model")
	}
}

// TestProvenContractSkipsSelfEvaluation. Asking a model to second-guess a
// passing test suite adds a call and can only make the answer worse.
func TestProvenContractSkipsSelfEvaluation(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"done"}}
	contract := &Contract{
		Criteria: []string{"go test ./... passes"},
		Verify: func(ctx context.Context) Verdict {
			return Verdict{Passed: true, Checked: 3}
		},
	}

	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	res, err := l.RunWithContract(context.Background(), "add a test", nil, contract)
	if err != nil {
		t.Fatalf("RunWithContract: %v", err)
	}
	if got := client.calls; got != 1 {
		t.Errorf("callCount = %d, want 1 — proven acceptance should not also pay for a self-eval", got)
	}
	if !res.Completed {
		t.Error("a run with passing acceptance checks must be recorded as completed")
	}
}

// TestUnenforceableContractFallsBackToSelfEval. A conversational goal has
// nothing machine-checkable about it, and inventing a predicate would be worse
// than admitting there isn't one.
func TestUnenforceableContractFallsBackToSelfEval(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"a mutex is a lock", "GOAL_STATUS: DONE"}}
	contract := &Contract{Criteria: []string{"the explanation is correct"}} // no Verify

	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	res, err := l.RunWithContract(context.Background(), "explain mutexes", nil, contract)
	if err != nil {
		t.Fatalf("RunWithContract: %v", err)
	}
	if got := client.calls; got != 2 {
		t.Errorf("callCount = %d, want 2 (think, self-eval) — an unenforceable contract must not skip the fallback", got)
	}
	if !res.Completed {
		t.Error("Completed = false, want true")
	}
}

// TestContractCriteriaAreShownToTheModel. Telling an agent the test it must
// pass is not cheating; it is the difference between working toward a target
// and guessing at one.
func TestContractCriteriaAreShownToTheModel(t *testing.T) {
	var sawCriteria bool
	client := &fakeLLMClient{
		responses: []string{"done"},
		onRequest: func(req *core.CompletionRequest) {
			for _, m := range req.Messages {
				if m.Role == core.RoleSystem && strings.Contains(m.ContentString(), "npm run lint passes") {
					sawCriteria = true
				}
			}
		},
	}
	contract := &Contract{
		Criteria: []string{"npm run lint passes"},
		Verify:   func(ctx context.Context) Verdict { return Verdict{Passed: true, Checked: 1} },
	}

	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	if _, err := l.RunWithContract(context.Background(), "tidy the code", nil, contract); err != nil {
		t.Fatalf("RunWithContract: %v", err)
	}
	if !sawCriteria {
		t.Error("the acceptance criteria were never shown to the model")
	}
}

// TestVerdictProvenDistinguishesAbsenceOfEvidence. "Nothing contradicted the
// claim" is not the same fact as "the criteria were run and held", and
// conflating them is how a green tick stops meaning anything.
func TestVerdictProvenDistinguishesAbsenceOfEvidence(t *testing.T) {
	if (Verdict{Passed: true, Checked: 0}).Proven() {
		t.Error("a verdict that checked nothing must not count as proven")
	}
	if !(Verdict{Passed: true, Checked: 1}).Proven() {
		t.Error("a passing verdict with a real check must count as proven")
	}
	if (Verdict{Passed: false, Checked: 3}).Proven() {
		t.Error("a failing verdict must never count as proven")
	}
}
