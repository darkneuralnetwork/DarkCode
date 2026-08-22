package orchestrator

// finding2b_plan_gate_trivial_test.go — QA audit Finding 2b: plan_approval:
// "always" never engaged for a "trivial"-classified task, because only the
// decomposed-plan path (Step 5.5) ever consulted it — Step 4's trivial
// fast path went straight to execution. These tests confirm a trivial task
// now pauses for approval under "always" (with zero LLM calls spent doing
// so, since building the one-node preview needs no model call), executes
// only after approval, and is unaffected when the policy isn't "always".

import (
	"context"
	"strings"
	"testing"
)

func TestTrivialTaskPausesForApprovalWhenPlanApprovalAlways(t *testing.T) {
	client := &fakeLLMClient{name: "primary", responses: []string{"the function was added"}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetPlanControls("always", "light")

	goal := "add a helper function to utils.go"
	out, err := deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !deps.Kernel.PlanAwaitingApproval() {
		t.Fatal("expected a trivial task under plan_approval:always to pause for approval")
	}
	if client.callCount() != 0 {
		t.Fatalf("building the trivial-task preview should need no LLM call, got %d", client.callCount())
	}
	if !strings.Contains(out, "approve") {
		t.Fatalf("expected an approval prompt in the preview, got:\n%s", out)
	}

	// Approving now runs it — this is where the worker's one LLM call happens.
	out, err = deps.Kernel.Execute(context.Background(), "approve")
	if err != nil {
		t.Fatalf("Execute(approve): %v", err)
	}
	if deps.Kernel.PlanAwaitingApproval() {
		t.Fatal("plan still pending after approval")
	}
	if client.callCount() == 0 {
		t.Fatal("approving the plan should have run the task, making at least one LLM call")
	}
	if !strings.Contains(out, "added") {
		t.Fatalf("expected the approved task's real output, got:\n%s", out)
	}
}

func TestTrivialTaskStillDirectWhenPlanApprovalIsNotAlways(t *testing.T) {
	client := &fakeLLMClient{name: "primary", responses: []string{"done"}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetPlanControls("auto", "light")

	out, err := deps.Kernel.Execute(context.Background(), "add a helper function to utils.go")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deps.Kernel.PlanAwaitingApproval() {
		t.Fatal("plan_approval:auto must not gate a trivial task — behavior must be unchanged")
	}
	if client.callCount() == 0 {
		t.Fatal("expected the trivial task to execute directly, making an LLM call")
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("expected the direct execution's real output, got:\n%s", out)
	}
}
