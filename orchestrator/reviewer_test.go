package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/plan"
)

func provenGraph() *plan.Graph {
	return &plan.Graph{Goal: "add retries", Nodes: []*plan.Node{{
		ID: "T1", Proof: []plan.Proof{{Criterion: "tests", Command: "go test ./...", Passed: true}},
	}}}
}

func failingGraph() *plan.Graph {
	return &plan.Graph{Goal: "add retries", Nodes: []*plan.Node{{
		ID: "T1", Proof: []plan.Proof{{Criterion: "tests", Command: "go test ./...", Passed: false}},
	}}}
}

// TestReviewerIsOffByDefault. An extra model call on every successful run is a
// real cost on a metered tier, for advice nobody asked for.
func TestReviewerIsOffByDefault(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"some advice"}}
	deps := newTestKernel(t, client)

	got := deps.Kernel.reviewProvenWork(context.Background(), "goal", "output", provenGraph())
	if got != "" {
		t.Errorf("reviewer produced output while disabled: %q", got)
	}
	if client.callCount() != 0 {
		t.Errorf("reviewer spent %d call(s) while disabled", client.callCount())
	}
}

// TestReviewerRunsOnlyOnProvenWork. Unproven work has a more urgent problem
// than style, and burying a failure under suggestions is the wrong output.
func TestReviewerRunsOnlyOnProvenWork(t *testing.T) {
	cases := map[string]*plan.Graph{
		"failing checks":   failingGraph(),
		"no checks at all": {Nodes: []*plan.Node{{ID: "T1"}}},
		"no graph":         nil,
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			client := &fakeLLMClient{name: "fake", responses: []string{"advice"}}
			deps := newTestKernel(t, client)
			deps.Kernel.SetReviewer(true)

			if got := deps.Kernel.reviewProvenWork(context.Background(), "goal", "out", g); got != "" {
				t.Errorf("reviewed unproven work: %q", got)
			}
			if client.callCount() != 0 {
				t.Errorf("spent %d call(s) reviewing unproven work", client.callCount())
			}
		})
	}
}

// TestReviewerAppendsAdviceOnProvenWork — the working path.
func TestReviewerAppendsAdviceOnProvenWork(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{
		"- extract the retry backoff into its own function so the policy is testable",
	}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetReviewer(true)

	got := deps.Kernel.reviewProvenWork(context.Background(), "add retries", "done", provenGraph())
	if got == "" {
		t.Fatal("no review produced for proven work")
	}
	if !strings.Contains(got, "retry backoff") {
		t.Errorf("review lost the advice: %q", got)
	}
	if !strings.Contains(got, "checks passed") {
		t.Error("review should say the checks already passed, so it reads as optional")
	}
}

// TestReviewerCannotFailARun. A review that can turn a proven pass into a
// failure is a second gate wearing a different name.
func TestReviewerCannotFailARun(t *testing.T) {
	client := &fakeLLMClient{name: "fake", err: context.DeadlineExceeded}
	deps := newTestKernel(t, client)
	deps.Kernel.SetReviewer(true)

	// An unreachable reviewer must produce nothing and panic nothing.
	if got := deps.Kernel.reviewProvenWork(context.Background(), "goal", "out", provenGraph()); got != "" {
		t.Errorf("a failed review produced output: %q", got)
	}
}

// TestReviewerStaysQuietWhenItHasNothingToSay. Padding is how automated review
// becomes noise people learn to skip.
func TestReviewerStaysQuietWhenItHasNothingToSay(t *testing.T) {
	for _, reply := range []string{"NO SUGGESTIONS", "no suggestions.", "  ", "fine"} {
		client := &fakeLLMClient{name: "fake", responses: []string{reply}}
		deps := newTestKernel(t, client)
		deps.Kernel.SetReviewer(true)

		if got := deps.Kernel.reviewProvenWork(context.Background(), "goal", "out", provenGraph()); got != "" {
			t.Errorf("reply %q produced a review section: %q", reply, got)
		}
	}
}

// TestGraphProven pins the predicate the gate above depends on.
func TestGraphProven(t *testing.T) {
	if !graphProven(provenGraph()) {
		t.Error("a graph with a passing check should be proven")
	}
	if graphProven(failingGraph()) {
		t.Error("a graph with a failing check is not proven")
	}
	// Prose criteria are recorded with no Command and are not evidence.
	prose := &plan.Graph{Nodes: []*plan.Node{{ID: "T1",
		Proof: []plan.Proof{{Criterion: "reads clearly", Passed: true}}}}}
	if graphProven(prose) {
		t.Error("an unverifiable prose criterion counted as proof")
	}
}
