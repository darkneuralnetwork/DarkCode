package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
)

// newTestKernelConsensus builds a Kernel in consensus mode with two distinct
// registered model names sharing the same fake client, so router.Consensus
// has both a primary and an "other" to fan out to.
func newTestKernelConsensus(t *testing.T, client *fakeLLMClient) *testKernelDeps {
	t.Helper()
	deps := newTestKernelWithMode(t, core.RouteConsensus, client)
	// Register a second, distinct model name so Consensus() has an "other" to
	// fan out to (a single registered name only ever hits the primary-only
	// fallback branch).
	deps.Router.RegisterModel(core.ModelTierCoding, client, "secondary-model")
	return deps
}

func TestRunConsensusRequiresConsensusMode(t *testing.T) {
	client := &fakeLLMClient{name: "primary", responses: []string{"answer"}}
	deps := newTestKernel(t, client) // single mode

	_, err := deps.Kernel.runConsensus(context.Background(), "explain how caching works", "")
	if err == nil {
		t.Fatal("expected an error when the router isn't in consensus mode")
	}
}

func TestRunConsensusPrimaryOnlyFallback(t *testing.T) {
	client := &fakeLLMClient{name: "primary", responses: []string{"the primary's answer"}}
	deps := newTestKernelWithMode(t, core.RouteConsensus, client) // only one registered name

	out, err := deps.Kernel.runConsensus(context.Background(), "explain how caching works", "")
	if err != nil {
		t.Fatalf("runConsensus: %v", err)
	}
	if out != "the primary's answer" {
		t.Errorf("out = %q, want the primary-only fallback answer", out)
	}
}

func TestRunConsensusFansOutAndSynthesizes(t *testing.T) {
	client := &fakeLLMClient{name: "shared", responses: []string{
		"contributor response 1", "contributor response 2", "final synthesized answer",
	}}
	deps := newTestKernelConsensus(t, client)

	out, err := deps.Kernel.runConsensus(context.Background(), "explain how caching works", "")
	if err != nil {
		t.Fatalf("runConsensus: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty synthesized answer")
	}
	if client.callCount() < 2 {
		t.Errorf("callCount = %d, want at least 2 (fan-out + synthesis)", client.callCount())
	}
}

func TestRunConsensusPrependsPreambleAsSystemMessage(t *testing.T) {
	var sawPreamble bool
	client := &fakeLLMClient{name: "primary", respFunc: func(idx int, req *core.CompletionRequest) string {
		for _, m := range req.Messages {
			if m.Role == core.RoleSystem && strings.Contains(m.ContentString(), "General mode") {
				sawPreamble = true
			}
		}
		return "answer"
	}}
	deps := newTestKernelWithMode(t, core.RouteConsensus, client)

	_, err := deps.Kernel.runConsensus(context.Background(), "hello", "General mode: no tools available.")
	if err != nil {
		t.Fatalf("runConsensus: %v", err)
	}
	if !sawPreamble {
		t.Error("expected the preamble to be prepended as a system message visible to the model")
	}
}

func TestRunConsensusOnOutputGroundsInToolTrace(t *testing.T) {
	var sawGrounding bool
	client := &fakeLLMClient{name: "primary", respFunc: func(idx int, req *core.CompletionRequest) string {
		for _, m := range req.Messages {
			if strings.Contains(m.ContentString(), "ALREADY executed") {
				sawGrounding = true
			}
		}
		return "refined answer"
	}}
	deps := newTestKernelWithMode(t, core.RouteConsensus, client)

	out, err := deps.Kernel.runConsensusOnOutput(context.Background(), "goal", "the agent's answer", "write_file(main.go) -> success")
	if err != nil {
		t.Fatalf("runConsensusOnOutput: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty refined answer")
	}
	if !sawGrounding {
		t.Error("expected the tool trace to ground the review prompt so reviewers can't hallucinate the agent has no tool access")
	}
}

func TestMergeWithConsensusFallsBackOnRouterError(t *testing.T) {
	client := &fakeLLMClient{name: "primary"}
	deps := newTestKernel(t, client) // single mode -> Consensus() always errors "not in consensus mode"

	results := []*core.SubAgentResult{
		{Output: "output A", Success: true},
		{Output: "output B", Success: true},
	}
	merged, err := deps.Kernel.mergeWithConsensus(context.Background(), results, "goal")
	if err != nil {
		t.Fatalf("mergeWithConsensus should fall back, not propagate the router error: %v", err)
	}
	if !strings.Contains(merged, "output A") || !strings.Contains(merged, "output B") {
		t.Errorf("fallback merge = %q, want it to contain both sub-agent outputs", merged)
	}
}

func TestMergeWithConsensusSynthesizesWhenAvailable(t *testing.T) {
	client := &fakeLLMClient{name: "shared", responses: []string{"contribution", "consensus-synthesized merge"}}
	deps := newTestKernelConsensus(t, client)

	results := []*core.SubAgentResult{
		{Output: "output A", Success: true},
		{Output: "output B", Success: true},
	}
	merged, err := deps.Kernel.mergeWithConsensus(context.Background(), results, "goal")
	if err != nil {
		t.Fatalf("mergeWithConsensus: %v", err)
	}
	if merged == "" {
		t.Fatal("expected a non-empty merged result")
	}
}

// Sanity check that the test router itself behaves like the real thing: a
// model registered under a second name is visible to router.Consensus as an
// "other" contributor, not silently deduped away by the shared client
// pointer.
func TestTestRouterRegistersDistinctModelNames(t *testing.T) {
	client := &fakeLLMClient{name: "shared"}
	rtr := router.NewRouter(core.RouteConsensus, nil)
	rtr.RegisterModel(core.ModelTierCoding, client, "primary-model")
	rtr.MarkPrimary("primary-model")
	rtr.RegisterModel(core.ModelTierCoding, client, "secondary-model")

	if rtr.ModelCount() < 2 {
		t.Fatalf("ModelCount() = %d, want >= 2", rtr.ModelCount())
	}
}
