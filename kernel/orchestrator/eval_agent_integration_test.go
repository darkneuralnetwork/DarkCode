package orchestrator

// eval_agent_integration_test.go proves kernel/eval/agent's Run function
// (and the wiring pattern live_test.go documents for a real model) actually
// works against a genuine *Kernel — not just the scripted-executor unit
// tests kernel/eval/agent itself has. This exercises the real path a live
// `make eval-agent` run takes: Kernel.Execute → real tool dispatch → a real
// write_file call → the harness's own artifact check — with a scripted LLM
// client standing in for the model (same fakeLLMClient +
// tools.RegisterBuiltinTools pattern finding1_regression_test.go already
// proved drives a real write_file call), so it runs in the normal
// (non-live, no-credentials) test suite.

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/security"
	evalagent "github.com/darkcode/kernel/eval/agent"
	"github.com/darkcode/tools/tools"
)

// TestEvalAgentRunPassesAgainstARealKernel is the integration proof for
// kernel/eval/agent.Run: given a Kernel whose model is scripted to write the
// expected artifact, the harness reports the task as passed — the same
// pass/fail logic a live run against a real model would apply.
func TestEvalAgentRunPassesAgainstARealKernel(t *testing.T) {
	client := &fakeLLMClient{
		name: "fake-primary",
		toolCallsFunc: func(idx int) []core.ToolCall {
			if idx != 0 {
				return nil
			}
			return []core.ToolCall{{
				ID:   "call1",
				Type: "function",
				Function: core.FunctionCall{
					Name:      "write_file",
					Arguments: `{"path":"hello.txt","content":"hello world"}`,
				},
			}}
		},
		respFunc: func(idx int, req *core.CompletionRequest) string {
			if idx == 0 {
				return ""
			}
			return "Wrote the file."
		},
	}
	deps := newTestKernel(t, client)
	tools.RegisterBuiltinTools(deps.Registry, nil, nil, security.NewSandbox(nil))

	c := &evalagent.Corpus{Name: "integration", Tasks: []evalagent.Task{
		{ID: "write-hello", Goal: "create hello.txt containing hello world", Artifacts: []string{"hello.txt"}},
	}}

	s, err := evalagent.Run(context.Background(), "fake", c, deps.Kernel, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Passed != 1 {
		t.Fatalf("Passed = %d, want 1: failures=%v", s.Passed, s.Failures)
	}
}
