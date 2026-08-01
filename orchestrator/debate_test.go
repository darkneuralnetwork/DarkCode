package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
)

func twoWayConflict() *core.ConsensusResult {
	return &core.ConsensusResult{
		Synthesized: "a merged answer",
		Conflict:    true,
		Contributions: []core.ModelContribution{
			{Model: "model-a", Role: "analyst", Output: "the timeout is 30 seconds"},
			{Model: "model-b", Role: "skeptic", Output: "no, the timeout is 5 seconds"},
		},
	}
}

// TestDebateIsOffByDefault. Two extra calls on a metered tier for something
// nobody asked for is exactly the cost this design exists to avoid.
func TestDebateIsOffByDefault(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"critique", "critique", "settled"}}
	deps := newTestKernel(t, client)

	out := deps.Kernel.resolveByDebate(context.Background(), "how long is the timeout", twoWayConflict())
	if out.Ran {
		t.Error("debate ran while disabled")
	}
	if client.callCount() != 0 {
		t.Errorf("debate spent %d call(s) while disabled", client.callCount())
	}
}

// TestDebateNeedsTwoRealPositions — one model, or one that errored, is not a
// disagreement.
func TestDebateNeedsTwoRealPositions(t *testing.T) {
	cases := map[string]*core.ConsensusResult{
		"single contribution": {Conflict: true, Contributions: []core.ModelContribution{
			{Model: "a", Output: "only answer"}}},
		"one errored": {Conflict: true, Contributions: []core.ModelContribution{
			{Model: "a", Output: "answer"}, {Model: "b", Error: "429 rate limited"}}},
		"one empty": {Conflict: true, Contributions: []core.ModelContribution{
			{Model: "a", Output: "answer"}, {Model: "b", Output: "   "}}},
	}
	for name, cr := range cases {
		t.Run(name, func(t *testing.T) {
			client := &fakeLLMClient{name: "fake", responses: []string{"x"}}
			deps := newTestKernel(t, client)
			deps.Kernel.SetDebate(true)

			if out := deps.Kernel.resolveByDebate(context.Background(), "q", cr); out.Ran {
				t.Error("debated without two usable positions")
			}
			if client.callCount() != 0 {
				t.Errorf("spent %d call(s) on a non-disagreement", client.callCount())
			}
		})
	}
}

// TestDebateRunsExactlyOneRound. Accuracy plateaus at two or three rounds and
// drift compounds per round, so the cap is the design rather than a limitation.
func TestDebateRunsExactlyOneRound(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{
		"A says B is wrong about the unit",
		"B says A misread the config",
		"the timeout is 5 seconds; A misread milliseconds for seconds",
	}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetDebate(true)

	out := deps.Kernel.resolveByDebate(context.Background(), "how long is the timeout", twoWayConflict())
	if !out.Ran {
		t.Fatal("debate did not run on a real conflict")
	}
	if out.Rounds != 1 {
		t.Errorf("ran %d rounds, want exactly 1", out.Rounds)
	}
	// Two critiques plus one settlement. More than that is a second round.
	if client.callCount() != 3 {
		t.Errorf("spent %d calls; one round is 2 critiques + 1 settlement", client.callCount())
	}
	if !strings.Contains(out.Resolved, "5 seconds") {
		t.Errorf("settlement lost the conclusion: %q", out.Resolved)
	}
}

// TestDebateAnchorsOnTheOriginalQuestion. Re-pinning the goal in every critique
// is the published mitigation for problem drift, and the reason one round is
// enough rather than merely affordable.
func TestDebateAnchorsOnTheOriginalQuestion(t *testing.T) {
	const goal = "how long is the auth timeout"
	seen := 0
	client := &fakeLLMClient{
		name:      "fake",
		responses: []string{"c1", "c2", "settled"},
		respFunc: func(i int, req *core.CompletionRequest) string {
			for _, m := range req.Messages {
				if strings.Contains(m.ContentString(), goal) {
					seen++
					break
				}
			}
			return "reply"
		},
	}
	deps := newTestKernel(t, client)
	deps.Kernel.SetDebate(true)

	deps.Kernel.resolveByDebate(context.Background(), goal, twoWayConflict())
	if seen < 3 {
		t.Errorf("the question was re-pinned in only %d of 3 prompts", seen)
	}
}

// TestDebateSurvivesAnUnreachableModel. A failed exchange must fall back to the
// synthesis, not lose the answer.
func TestDebateSurvivesAnUnreachableModel(t *testing.T) {
	client := &fakeLLMClient{name: "fake", err: context.DeadlineExceeded}
	deps := newTestKernel(t, client)
	deps.Kernel.SetDebate(true)

	out := deps.Kernel.resolveByDebate(context.Background(), "q", twoWayConflict())
	if out.Ran {
		t.Error("reported a run that produced nothing")
	}
	if out.Resolved != "" {
		t.Errorf("produced an answer from a failed exchange: %q", out.Resolved)
	}
}

// TestDebateRecordsTheExchangeOnTheBus. The bus has carried a critique message
// kind since it was written and never sent one; the exchange being inspectable
// is what makes it auditable rather than a black box.
func TestDebateRecordsTheExchangeOnTheBus(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"crit a", "crit b", "settled"}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetDebate(true)

	before := deps.Kernel.agentBus.MessageCount()
	deps.Kernel.resolveByDebate(context.Background(), "q", twoWayConflict())
	after := deps.Kernel.agentBus.MessageCount()

	if after-before != 2 {
		t.Errorf("bus recorded %d message(s), want 2 (one per direction)", after-before)
	}
	for _, m := range deps.Kernel.agentBus.RecentHistory(2) {
		if m.Kind != core.MsgCritiqueRequest {
			t.Errorf("message kind = %q, want %q", m.Kind, core.MsgCritiqueRequest)
		}
	}
}

// TestGroundedCheckBeatsDebate is the gate that keeps this affordable. When the
// knowledge graph can check the claims, the check settles it for zero extra
// model calls — the exchange is strictly the fallback for when it cannot.
func TestGroundedCheckBeatsDebate(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"critique", "critique", "settled"}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetDebate(true)

	kg, ok := deps.Memory.KG().(*memory.KnowledgeGraph)
	if !ok {
		t.Fatal("test kernel has no knowledge graph")
	}
	if err := kg.AddNode(&core.KGNode{ID: "file:permission/gate.go", Label: "permission/gate.go",
		Type: core.KGNodeFile, Confidence: 1,
		Properties: map[string]string{"origin": "code_index"}}); err != nil {
		t.Fatal(err)
	}
	if err := kg.AddNode(&core.KGNode{ID: "symbol:Gate@permission/gate.go", Label: "Gate",
		Type: core.KGNodeSymbol, Confidence: 1, Provenance: "permission/gate.go:10",
		Properties: map[string]string{"origin": "code_index", "kind": "type", "references": "1"}}); err != nil {
		t.Fatal(err)
	}

	cr := &core.ConsensusResult{
		Synthesized: "Gate is defined in permission/gate.go",
		Conflict:    true, // they disagreed, but the claims are checkable
		Contributions: []core.ModelContribution{
			{Model: "model-a", Output: "Gate is defined in permission/gate.go"},
			{Model: "model-b", Output: "Gate is defined in memory/store.go"},
		},
	}

	out := deps.Kernel.adjudicateCtx(context.Background(), "where is Gate defined", cr)
	if client.callCount() != 0 {
		t.Errorf("debated %d call(s) when the graph could check the claims", client.callCount())
	}
	if !strings.Contains(out, "permission/gate.go") {
		t.Errorf("adjudication picked an unsupported answer: %q", out)
	}
}
