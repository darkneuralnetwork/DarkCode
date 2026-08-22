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

// TestDebateIsOffByDefaultThroughTheKernel — the kernel wires the runtime
// toggle into the adjudicator, so "off" has to survive that wiring, not just
// the component's own default. The exchange itself is tested in package
// adjudicate, which is where it now lives.
func TestDebateIsOffByDefaultThroughTheKernel(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"critique", "critique", "settled"}}
	deps := newTestKernel(t, client)

	out := deps.Kernel.adjudicateCtx(context.Background(), "how long is the timeout", twoWayConflict())
	if client.callCount() != 0 {
		t.Errorf("debate spent %d call(s) while disabled", client.callCount())
	}
	if out != "a merged answer" {
		t.Errorf("answer = %q, want the synthesis", out)
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
