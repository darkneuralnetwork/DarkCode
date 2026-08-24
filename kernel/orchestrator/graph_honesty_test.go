package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/dag"
)

// A graph run where one task failed and another was blocked behind it must not
// read as a clean success. The mechanism exists — failed and cancelled nodes
// are appended to the answer and the recorded outcome is derived from
// FailedNodes — but nothing pinned it, and it is the difference between the
// user seeing what went wrong and being told the work is done.

// graphWithAChain builds a → b → c so cancelling descendants has something to
// cancel.
func graphWithAChain(t *testing.T) *dag.DAG {
	t.Helper()
	d := dag.NewDAG()
	deps := map[string][]string{"a": nil, "b": {"a"}, "c": {"b"}}
	for _, id := range []string{"a", "b", "c"} {
		n := &core.TaskNode{ID: id, Name: "task " + id, Status: core.TaskPending, Dependencies: deps[id]}
		if err := d.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// TestAFailedNodeBlocksItsDependentsRatherThanRunningThem. Running c on a
// missing input from b produces work built on nothing, which is worse than not
// running it: it looks like progress.
func TestAFailedNodeBlocksItsDependentsRatherThanRunningThem(t *testing.T) {
	d := graphWithAChain(t)

	if err := d.UpdateStatus("a", core.TaskFailed); err != nil {
		t.Fatal(err)
	}
	cancelled := d.CancelDescendants("a")

	if len(cancelled) != 2 {
		t.Errorf("cancelled %v, want both dependents of a", cancelled)
	}
	for _, id := range []string{"b", "c"} {
		n, ok := d.GetNode(id)
		if !ok {
			t.Fatalf("%s vanished", id)
		}
		if n.Status != core.TaskCancelled {
			t.Errorf("%s status = %v, want cancelled", id, n.Status)
		}
	}
}

// TestCancelDescendantsLeavesFinishedWorkAlone. A task that already completed
// before an unrelated sibling failed must keep its result — rewriting it to
// cancelled would discard work that was actually done.
func TestCancelDescendantsLeavesFinishedWorkAlone(t *testing.T) {
	d := graphWithAChain(t)
	if err := d.MarkCompleted("b"); err != nil {
		t.Fatal(err)
	}

	d.UpdateStatus("a", core.TaskFailed)
	d.CancelDescendants("a")

	n, _ := d.GetNode("b")
	if n.Status != core.TaskCompleted {
		t.Errorf("a completed task was rewritten to %v", n.Status)
	}
	// c never ran, so it is still fair game.
	if c, _ := d.GetNode("c"); c.Status != core.TaskCancelled {
		t.Errorf("c status = %v, want cancelled", c.Status)
	}
}

// TestFailedNodesReportsWhatToTellTheUser. The answer's "Incomplete tasks"
// block is built from this; an empty list would render a half-failed run as a
// clean one.
func TestFailedNodesReportsWhatToTellTheUser(t *testing.T) {
	d := graphWithAChain(t)
	if len(d.FailedNodes()) != 0 {
		t.Fatal("a fresh graph reports failures")
	}

	d.UpdateStatus("a", core.TaskFailed)
	d.SetError("a", "go build: exit status 1")

	failed := d.FailedNodes()
	if len(failed) != 1 {
		t.Fatalf("FailedNodes returned %d entries, want 1", len(failed))
	}
	if failed[0].ID != "a" {
		t.Errorf("reported %q as failed", failed[0].ID)
	}
	if !strings.Contains(failed[0].Error, "exit status 1") {
		t.Errorf("the reason was lost: %q — the user is told a task failed but "+
			"not why", failed[0].Error)
	}
}

// TestAPartlyFailedGraphIsNotRecordedAsSuccess. recordOutcome takes
// len(FailedNodes()) == 0 as the success flag, and that flag decides what goes
// into episodic memory. Recording a failed run as successful is how abort text
// became a cached answer once before.
func TestAPartlyFailedGraphIsNotRecordedAsSuccess(t *testing.T) {
	d := graphWithAChain(t)
	d.MarkCompleted("a")
	d.UpdateStatus("b", core.TaskFailed)
	d.SetError("b", "boom")
	d.CancelDescendants("b")

	if success := len(d.FailedNodes()) == 0; success {
		t.Error("a graph with a failed node reports success")
	}

	// And the cancelled dependent is visible too, so the report can say why c
	// never ran rather than leaving it silently absent.
	var cancelled int
	for _, n := range d.AllNodes() {
		if n.Status == core.TaskCancelled {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Error("no cancelled nodes recorded, so the report cannot explain the gap")
	}
}

// TestExecuteDAGOnAnEmptyGraph. Planning can legitimately return nothing to do;
// that must not be an error or a panic.
func TestExecuteDAGOnAnEmptyGraph(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)

	results, err := deps.Kernel.executeDAG(context.Background(), dag.NewDAG(), "nothing to do")
	if err != nil {
		t.Errorf("an empty graph errored: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("an empty graph produced %d results", len(results))
	}
}
