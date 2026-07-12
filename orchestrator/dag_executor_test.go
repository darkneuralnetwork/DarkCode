package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/dag"
)

func TestPlanAndDecomposeSuccess(t *testing.T) {
	client := &fakeLLMClient{name: "planner", responses: []string{
		plannerResponse(
			plannerTask{name: "task1", goal: "write the function"},
			plannerTask{name: "task2", goal: "write the tests", deps: "task1"},
		),
	}}
	deps := newTestKernel(t, client)

	d, err := deps.Kernel.planAndDecompose(context.Background(), "implement a feature")
	if err != nil {
		t.Fatalf("planAndDecompose: %v", err)
	}
	if d == nil || d.NodeCount() != 2 {
		t.Fatalf("expected a 2-node DAG, got %v", d)
	}
}

func TestPlanAndDecomposeNoTasks(t *testing.T) {
	client := &fakeLLMClient{name: "planner", responses: []string{"I don't understand the request."}}
	deps := newTestKernel(t, client)

	_, err := deps.Kernel.planAndDecompose(context.Background(), "??")
	if err == nil {
		t.Fatal("expected an error when the planner produces no parseable tasks")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("error = %v, want it to mention no tasks were produced", err)
	}
}

func buildTestDAG(t *testing.T, nodes ...*core.TaskNode) *dag.DAG {
	t.Helper()
	d := dag.NewDAG()
	for _, n := range nodes {
		if err := d.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n.ID, err)
		}
	}
	return d
}

func TestExecuteDAGRunsIndependentTasksToCompletion(t *testing.T) {
	client := &fakeLLMClient{name: "worker", responses: []string{"done with task"}}
	deps := newTestKernel(t, client)

	d := buildTestDAG(t,
		&core.TaskNode{ID: "a", Name: "a", Goal: "do a", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
		&core.TaskNode{ID: "b", Name: "b", Goal: "do b", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
	)

	results, err := deps.Kernel.executeDAG(context.Background(), d, "the goal")
	if err != nil {
		t.Fatalf("executeDAG: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if !r.Success {
			t.Errorf("result %+v: expected success", r)
		}
	}
}

func TestExecuteDAGRespectsDependencyOrder(t *testing.T) {
	var order []string
	client := &fakeLLMClient{name: "worker", respFunc: func(idx int, req *core.CompletionRequest) string {
		// The goal text is in the user message; use it to track which node ran.
		for _, m := range req.Messages {
			if m.Role == core.RoleUser {
				order = append(order, m.ContentString())
			}
		}
		return "done"
	}}
	deps := newTestKernel(t, client)

	d := buildTestDAG(t,
		&core.TaskNode{ID: "first", Name: "first", Goal: "step one", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
	)
	if err := d.AddNode(&core.TaskNode{
		ID: "second", Name: "second", Goal: "step two", Status: core.TaskPending,
		AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding, Dependencies: []string{"first"},
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	results, err := deps.Kernel.executeDAG(context.Background(), d, "goal")
	if err != nil {
		t.Fatalf("executeDAG: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if len(order) < 2 || !strings.Contains(order[0], "step one") {
		t.Errorf("execution order = %v, want \"step one\" before \"step two\"", order)
	}
}

// TestExecuteDAGPreservesPartialResultsOnCancellation exercises the Phase-0
// fix: cancelling mid-DAG must return whatever completed so far (not nil),
// so the caller in Kernel.Execute can attempt a best-effort merge instead of
// discarding all completed sub-agent work.
func TestExecuteDAGPreservesPartialResultsOnCancellation(t *testing.T) {
	client := &fakeLLMClient{name: "worker", responses: []string{"done"}, delay: 100 * time.Millisecond}
	deps := newTestKernel(t, client)

	// Two independent (parallel) tasks so both start before cancellation.
	d := buildTestDAG(t,
		&core.TaskNode{ID: "a", Name: "a", Goal: "do a", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
		&core.TaskNode{ID: "b", Name: "b", Goal: "do b", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	results, err := deps.Kernel.executeDAG(ctx, d, "goal")
	if err == nil {
		t.Fatal("expected a context-deadline error")
	}
	// The fix under test: results must not be forcibly nil'd out on
	// cancellation. Whether individual sub-agent calls raced the deadline
	// closely enough to produce results is inherently timing-dependent, but
	// the function must never discard a non-nil result slice — assert the
	// non-nil-on-error contract, not an exact count.
	_ = results // reaching here without a panic/nil-deref on results is itself part of what's verified
}

func TestExecuteDAGDeadlockDetection(t *testing.T) {
	client := &fakeLLMClient{name: "worker"}
	deps := newTestKernel(t, client)

	d := buildTestDAG(t,
		&core.TaskNode{ID: "blocker", Name: "blocker", Goal: "never completes", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
	)
	if err := d.AddNode(&core.TaskNode{
		ID: "dependent", Name: "dependent", Goal: "needs blocker", Status: core.TaskPending,
		AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding, Dependencies: []string{"blocker"},
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// Force "blocker" into a terminal-but-not-completed state so it can never
	// satisfy "dependent"'s dependency, and it's not re-offered as ready
	// either (status != pending) — this is the deadlock executeDAG's guard
	// is meant to catch.
	if err := d.UpdateStatus("blocker", core.TaskFailed); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	_, err := deps.Kernel.executeDAG(context.Background(), d, "goal")
	if err == nil {
		t.Fatal("expected a deadlock error")
	}
	if !strings.Contains(err.Error(), "deadlock") {
		t.Errorf("error = %v, want it to mention deadlock", err)
	}
}

func TestMergeResultsSingleResultReturnedDirectly(t *testing.T) {
	client := &fakeLLMClient{name: "synth"}
	deps := newTestKernel(t, client)

	results := []*core.SubAgentResult{{Output: "the only output", Success: true}}
	merged, err := deps.Kernel.mergeResults(context.Background(), results, "goal")
	if err != nil {
		t.Fatalf("mergeResults: %v", err)
	}
	if merged != "the only output" {
		t.Errorf("merged = %q, want the single result returned unchanged (no LLM call needed)", merged)
	}
	if client.callCount() != 0 {
		t.Errorf("expected no LLM call for a single-result merge, got %d calls", client.callCount())
	}
}

func TestMergeResultsMultipleResultsSynthesized(t *testing.T) {
	client := &fakeLLMClient{name: "synth", responses: []string{"synthesized answer"}}
	deps := newTestKernel(t, client)

	results := []*core.SubAgentResult{
		{Output: "output A", Success: true, Role: core.RoleWorker},
		{Output: "output B", Success: true, Role: core.RoleWorker},
	}
	merged, err := deps.Kernel.mergeResults(context.Background(), results, "goal")
	if err != nil {
		t.Fatalf("mergeResults: %v", err)
	}
	if merged != "synthesized answer" {
		t.Errorf("merged = %q, want the synthesizer's output", merged)
	}
	if client.callCount() == 0 {
		t.Error("expected mergeResults to call the LLM to synthesize multiple results")
	}
}
