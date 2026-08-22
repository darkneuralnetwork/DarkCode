package orchestrator

import (
	"context"
	"testing"
)

// TestExecute_ObviousGeneralQuestion_SingleCall verifies the CLI-path cost
// guard: an obvious general question routes straight to the single-call
// no-tools path instead of the tool/worker pipeline (which the user's trace
// showed making ~4 LLM calls for "who is narendra modi?").
func TestExecute_ObviousGeneralQuestion_SingleCall(t *testing.T) {
	deps := newTestKernel(t, nil)

	out, err := deps.Kernel.Execute(context.Background(), "who is alan turing?")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty answer")
	}
	if n := deps.Client.callCount(); n != 1 {
		t.Fatalf("an obvious general question should take a single LLM call, got %d", n)
	}
}
