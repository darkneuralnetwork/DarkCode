package orchestrator

import (
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/loop"
	"github.com/darkcode/plan"
)

func nodeWithProof(id string, proofs ...plan.Proof) *plan.Node {
	return &plan.Node{ID: id, Name: id, Proof: proofs}
}

// TestPerNodeStatusFollowsEachNodesOwnProof is why this exists. Stamping one
// status across the graph made the Blueprint tab useless in the case that
// matters: a run where three tasks passed and one failed showed four failures,
// so there was nothing to look at to find which one to fix.
func TestPerNodeStatusFollowsEachNodesOwnProof(t *testing.T) {
	g := &plan.Graph{Nodes: []*plan.Node{
		nodeWithProof("T1", plan.Proof{Criterion: "build", Command: "go build ./...", Passed: true}),
		nodeWithProof("T2", plan.Proof{Criterion: "test", Command: "go test ./pkg", Passed: false}),
		nodeWithProof("T3", plan.Proof{Criterion: "lint", Command: "go vet ./...", Passed: true}),
	}}

	markGraphFrom(g, loop.Verdict{Passed: false, Checked: 3, Evidence: "boom"}, false)

	want := map[string]core.TaskStatus{
		"T1": core.TaskCompleted,
		"T2": core.TaskFailed,
		"T3": core.TaskCompleted,
	}
	for _, n := range g.Nodes {
		if n.Status != want[n.ID] {
			t.Errorf("%s status = %v, want %v", n.ID, n.Status, want[n.ID])
		}
	}
}

// TestNodeWithoutProofIsNotBlamed. A node whose own checks never ran must not
// be marked failed because a sibling failed — a red tick that means "something
// somewhere broke" is as useless as a green one that means nothing.
func TestNodeWithoutProofIsNotBlamed(t *testing.T) {
	g := &plan.Graph{Nodes: []*plan.Node{
		nodeWithProof("T1", plan.Proof{Criterion: "test", Command: "go test ./...", Passed: false}),
		nodeWithProof("T2"), // nothing machine-checkable
	}}

	markGraphFrom(g, loop.Verdict{Passed: false, Checked: 1}, false)

	if g.Nodes[0].Status != core.TaskFailed {
		t.Errorf("T1 = %v, want failed", g.Nodes[0].Status)
	}
	if g.Nodes[1].Status == core.TaskFailed {
		t.Error("T2 was blamed for a sibling's failure")
	}
}

// TestProseCriteriaAreNotEvidence. checkAcceptance records an unverifiable
// sentence as "not machine-checkable"; counting it as a pass is exactly the
// failure the acceptance path exists to remove.
func TestProseCriteriaAreNotEvidence(t *testing.T) {
	g := &plan.Graph{Nodes: []*plan.Node{
		nodeWithProof("T1", plan.Proof{Criterion: "the code is readable", Passed: false,
			Output: "not machine-checkable — recorded as unverified"}),
	}}

	markGraphFrom(g, loop.Verdict{Passed: true, Checked: 0}, true)

	if g.Nodes[0].Status == core.TaskFailed {
		t.Error("a prose criterion was treated as a failing check")
	}
}

// TestProvenRunCompletesUncheckedNodes: when the contract as a whole was proven
// and a node carried nothing of its own, the run is the evidence it has.
func TestProvenRunCompletesUncheckedNodes(t *testing.T) {
	g := &plan.Graph{Nodes: []*plan.Node{nodeWithProof("T1")}}
	markGraphFrom(g, loop.Verdict{Passed: true, Checked: 2}, true)
	if g.Nodes[0].Status != core.TaskCompleted {
		t.Errorf("T1 = %v, want completed under a proven verdict", g.Nodes[0].Status)
	}
}

// TestSharedCommandProvesEveryNodeThatDependsOnIt. Most nodes fall back to the
// same default predicate, and the memo used to skip them entirely — so every
// node after the first had no Proof and, once status went per node, would have
// looked unverified purely because a sibling was checked first.
func TestSharedCommandProvesEveryNodeThatDependsOnIt(t *testing.T) {
	shared := plan.Proof{Criterion: "build", Command: "go build ./...", Passed: true}
	g := &plan.Graph{Nodes: []*plan.Node{
		nodeWithProof("T1", shared),
		nodeWithProof("T2", shared),
		nodeWithProof("T3", shared),
	}}

	markGraphFrom(g, loop.Verdict{Passed: true, Checked: 1}, true)

	for _, n := range g.Nodes {
		if n.Status != core.TaskCompleted {
			t.Errorf("%s = %v, want completed — it depends on a check that passed", n.ID, n.Status)
		}
	}
}
