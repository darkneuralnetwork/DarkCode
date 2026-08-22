package dag

import (
	"testing"

	"github.com/darkcode/core"
)

func node(id string, deps ...string) *core.TaskNode {
	return &core.TaskNode{ID: id, Name: id, Status: core.TaskPending, Dependencies: deps}
}

func TestAddNodeValidation(t *testing.T) {
	d := NewDAG()
	if err := d.AddNode(node("a")); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if err := d.AddNode(node("a")); err == nil {
		t.Error("duplicate node ID should error")
	}
	if err := d.AddNode(node("b", "missing")); err == nil {
		t.Error("node depending on a missing node should error")
	}
	if d.NodeCount() != 1 {
		t.Errorf("failed adds must not register: count = %d, want 1", d.NodeCount())
	}
}

func TestTopologicalOrder(t *testing.T) {
	d := NewDAG()
	// a -> b -> c (c depends on b depends on a)
	must(t, d.AddNode(node("a")))
	must(t, d.AddNode(node("b", "a")))
	must(t, d.AddNode(node("c", "b")))

	order := d.AllNodes()
	pos := map[string]int{}
	for i, n := range order {
		pos[n.ID] = i
	}
	if pos["a"] > pos["b"] || pos["b"] > pos["c"] {
		t.Errorf("topological order violated: %v", idsOf(order))
	}
	if d.HasCycle() {
		t.Error("acyclic DAG reported a cycle")
	}
}

func TestReadyProgression(t *testing.T) {
	d := NewDAG()
	must(t, d.AddNode(node("a")))
	must(t, d.AddNode(node("b", "a")))

	ready := d.GetReadyTasks(map[string]bool{})
	if len(ready) != 1 || ready[0].ID != "a" {
		t.Fatalf("only a should be ready first, got %v", idsOf(ready))
	}
	// b becomes ready only once a is completed.
	must(t, d.MarkCompleted("a"))
	ready = d.GetReadyTasks(map[string]bool{"a": true})
	if len(ready) != 1 || ready[0].ID != "b" {
		t.Fatalf("b should be ready after a completes, got %v", idsOf(ready))
	}
	if d.IsComplete() {
		t.Error("DAG with a pending node should not report complete")
	}
	must(t, d.MarkCompleted("b"))
	if !d.IsComplete() {
		t.Error("DAG with all nodes completed should report complete")
	}
	if d.CompletedCount() != 2 {
		t.Errorf("CompletedCount = %d, want 2", d.CompletedCount())
	}
}

func TestCancelDescendants(t *testing.T) {
	d := NewDAG()
	must(t, d.AddNode(node("a")))
	must(t, d.AddNode(node("b", "a")))
	must(t, d.AddNode(node("c", "b")))
	must(t, d.AddNode(node("d"))) // independent

	must(t, d.UpdateStatus("a", core.TaskFailed))
	cancelled := d.CancelDescendants("a")
	if len(cancelled) != 2 {
		t.Fatalf("cancelling a should cancel b and c, got %v", cancelled)
	}
	if n, _ := d.GetNode("d"); n.Status != core.TaskPending {
		t.Error("independent node d must not be cancelled")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func idsOf(nodes []*core.TaskNode) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}
