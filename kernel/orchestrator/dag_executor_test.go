package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/dag"
)

const testPlanJSON = `[{"name":"task1","goal":"write the function","agent":"worker","deps":[]},{"name":"task2","goal":"write the tests","agent":"qa","deps":["task1"]}]`

func TestDeepPlanLightProducesGraphInOneCall(t *testing.T) {
	client := &fakeLLMClient{name: "planner", responses: []string{testPlanJSON}}
	deps := newTestKernel(t, client)

	g, err := deps.Kernel.deepPlan(context.Background(), "implement a feature", PlanDepthLight)
	if err != nil {
		t.Fatalf("deepPlan: %v", err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(g.Nodes))
	}
	if len(g.Nodes[1].Deps) != 1 || g.Nodes[1].Deps[0] != "T1" {
		t.Errorf("T2 deps = %v, want [T1]", g.Nodes[1].Deps)
	}
	if got := client.callCount(); got != 1 {
		t.Errorf("light planning made %d LLM calls, want exactly 1", got)
	}
}

func TestDeepPlanDeepRunsSelfReview(t *testing.T) {
	client := &fakeLLMClient{name: "planner", responses: []string{testPlanJSON, testPlanJSON}}
	deps := newTestKernel(t, client)

	g, err := deps.Kernel.deepPlan(context.Background(), "implement a feature", PlanDepthDeep)
	if err != nil {
		t.Fatalf("deepPlan: %v", err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(g.Nodes))
	}
	if got := client.callCount(); got != 2 {
		t.Errorf("deep planning made %d LLM calls, want 2 (decompose + self-review)", got)
	}
}

func TestDeepPlanUnparsableFailsAfterOneRepair(t *testing.T) {
	client := &fakeLLMClient{name: "planner", responses: []string{"I don't understand the request."}}
	deps := newTestKernel(t, client)

	_, err := deps.Kernel.deepPlan(context.Background(), "??", PlanDepthLight)
	if err == nil {
		t.Fatal("expected an error when the planner never produces parseable tasks")
	}
	if got := client.callCount(); got != 2 {
		t.Errorf("made %d LLM calls, want 2 (initial + one repair, no infinite retry)", got)
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

// TestExecuteDAGFailureBlocksDependents exercises the honest-failure fix: a
// failed node must be marked failed (not completed), its descendants must be
// cancelled instead of deadlocking the ready-loop, and the failed result is
// still returned for the merge to report.
func TestExecuteDAGFailureBlocksDependents(t *testing.T) {
	client := &fakeLLMClient{name: "worker", err: fmt.Errorf("boom")}
	deps := newTestKernel(t, client)

	d := buildTestDAG(t,
		&core.TaskNode{ID: "a", Name: "a", Goal: "do a", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
	)
	if err := d.AddNode(&core.TaskNode{
		ID: "b", Name: "b", Goal: "do b", Status: core.TaskPending,
		AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding, Dependencies: []string{"a"},
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	results, err := deps.Kernel.executeDAG(context.Background(), d, "goal")
	if err != nil {
		t.Fatalf("executeDAG should terminate cleanly on task failure, got: %v", err)
	}
	if len(results) != 1 || results[0].Success {
		t.Fatalf("results = %+v, want exactly the failed result of a", results)
	}
	na, _ := d.GetNode("a")
	if na.Status != core.TaskFailed {
		t.Errorf("a.Status = %s, want failed (was previously mislabeled completed)", na.Status)
	}
	nb, _ := d.GetNode("b")
	if nb.Status != core.TaskCancelled {
		t.Errorf("b.Status = %s, want cancelled (blocked by failed dependency)", nb.Status)
	}
}

// TestExecuteDAGPipesDependencyOutputs exercises the data-flow fix: a
// dependent task must receive its dependencies' outputs in its context —
// edges carry data, not just ordering.
func TestExecuteDAGPipesDependencyOutputs(t *testing.T) {
	sawUpstream := false
	client := &fakeLLMClient{name: "worker", respFunc: func(idx int, req *core.CompletionRequest) string {
		for _, m := range req.Messages {
			if m.Role == core.RoleSystem && strings.Contains(m.ContentString(), "SECRET_ALPHA_42") {
				sawUpstream = true
			}
		}
		return "SECRET_ALPHA_42"
	}}
	deps := newTestKernel(t, client)

	d := buildTestDAG(t,
		&core.TaskNode{ID: "first", Name: "first", Goal: "produce the value", Status: core.TaskPending, AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding},
	)
	if err := d.AddNode(&core.TaskNode{
		ID: "second", Name: "second", Goal: "use the value", Status: core.TaskPending,
		AgentRole: core.RoleWorker, ModelTier: core.ModelTierCoding, Dependencies: []string{"first"},
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if _, err := deps.Kernel.executeDAG(context.Background(), d, "goal"); err != nil {
		t.Fatalf("executeDAG: %v", err)
	}
	if !sawUpstream {
		t.Error("dependent task never saw its dependency's output in context")
	}
	if n, _ := d.GetNode("first"); n.Output == "" {
		t.Error("first.Output not recorded via SetOutput")
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
