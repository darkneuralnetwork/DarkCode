package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/core"
)

func newTestKG(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	t.Cleanup(kg.Shutdown)
	return kg
}

func mustAddNode(t *testing.T, kg *KnowledgeGraph, id string, typ core.KGNodeType) {
	t.Helper()
	if err := kg.AddNode(&core.KGNode{ID: id, Label: id, Type: typ}); err != nil {
		t.Fatalf("AddNode(%s): %v", id, err)
	}
}

func TestAddNodeAndGetNode(t *testing.T) {
	kg := newTestKG(t)
	mustAddNode(t, kg, "file:main.go", core.KGNodeFile)

	node, ok := kg.GetNode("file:main.go")
	if !ok {
		t.Fatal("expected node to exist")
	}
	if node.Type != core.KGNodeFile {
		t.Errorf("Type = %v, want %v", node.Type, core.KGNodeFile)
	}

	if _, ok := kg.GetNode("does-not-exist"); ok {
		t.Error("expected missing node lookup to fail")
	}
}

func TestAddEdgeRequiresBothNodes(t *testing.T) {
	kg := newTestKG(t)
	mustAddNode(t, kg, "a", core.KGNodeFile)

	err := kg.AddEdge(&core.KGEdge{From: "a", To: "b", Relation: core.KGRelDependsOn})
	if err == nil {
		t.Fatal("expected error for edge referencing a missing target node")
	}

	err = kg.AddEdge(&core.KGEdge{From: "missing", To: "a", Relation: core.KGRelDependsOn})
	if err == nil {
		t.Fatal("expected error for edge referencing a missing source node")
	}
}

// TestGetEdgesBidirectional exercises the adjacency-index-based GetEdges
// fix: it must return an edge for both endpoints (outgoing AND incoming),
// and must NOT return edges belonging to unrelated nodes.
func TestGetEdgesBidirectional(t *testing.T) {
	kg := newTestKG(t)
	mustAddNode(t, kg, "a", core.KGNodeFile)
	mustAddNode(t, kg, "b", core.KGNodeFile)
	mustAddNode(t, kg, "c", core.KGNodeFile)

	if err := kg.AddEdge(&core.KGEdge{From: "a", To: "b", Relation: core.KGRelDependsOn}); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	if err := kg.AddEdge(&core.KGEdge{From: "c", To: "a", Relation: core.KGRelUsedBy}); err != nil {
		t.Fatalf("AddEdge c->a: %v", err)
	}

	edgesA := kg.GetEdges("a")
	if len(edgesA) != 2 {
		t.Fatalf("GetEdges(a) = %d edges, want 2 (one outgoing to b, one incoming from c)", len(edgesA))
	}

	edgesB := kg.GetEdges("b")
	if len(edgesB) != 1 {
		t.Fatalf("GetEdges(b) = %d edges, want 1", len(edgesB))
	}

	// A node with no edges must return an empty result, not panic or return
	// unrelated edges.
	mustAddNode(t, kg, "isolated", core.KGNodeFile)
	if edges := kg.GetEdges("isolated"); len(edges) != 0 {
		t.Errorf("GetEdges(isolated) = %d edges, want 0", len(edges))
	}
}

func TestRemoveNodeCleansUpEdgesAndAdjacency(t *testing.T) {
	kg := newTestKG(t)
	mustAddNode(t, kg, "a", core.KGNodeFile)
	mustAddNode(t, kg, "b", core.KGNodeFile)
	if err := kg.AddEdge(&core.KGEdge{From: "a", To: "b", Relation: core.KGRelDependsOn}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if err := kg.RemoveNode("a"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}

	if _, ok := kg.GetNode("a"); ok {
		t.Error("node should be gone after RemoveNode")
	}
	if edges := kg.GetEdges("b"); len(edges) != 0 {
		t.Errorf("GetEdges(b) after removing a's edge = %d, want 0", len(edges))
	}
	related := kg.FindRelated("b")
	if len(related) != 0 {
		t.Errorf("FindRelated(b) after removing a = %d, want 0", len(related))
	}
}

func TestFindRelated(t *testing.T) {
	kg := newTestKG(t)
	mustAddNode(t, kg, "a", core.KGNodeFile)
	mustAddNode(t, kg, "b", core.KGNodeFile)
	mustAddNode(t, kg, "c", core.KGNodeFile)
	if err := kg.AddEdge(&core.KGEdge{From: "a", To: "b", Relation: core.KGRelDependsOn}); err != nil {
		t.Fatal(err)
	}
	if err := kg.AddEdge(&core.KGEdge{From: "a", To: "c", Relation: core.KGRelDependsOn}); err != nil {
		t.Fatal(err)
	}

	related := kg.FindRelated("a")
	if len(related) != 2 {
		t.Fatalf("FindRelated(a) = %d nodes, want 2", len(related))
	}
}

func TestKnowledgeGraphPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()

	kg1, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	mustAddNode(t, kg1, "a", core.KGNodeFile)
	mustAddNode(t, kg1, "b", core.KGNodeFile)
	if err := kg1.AddEdge(&core.KGEdge{From: "a", To: "b", Relation: core.KGRelDependsOn}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	kg1.Shutdown() // flushes pending writes

	kg2, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("reload NewKnowledgeGraph: %v", err)
	}
	defer kg2.Shutdown()

	if _, ok := kg2.GetNode("a"); !ok {
		t.Fatal("node 'a' did not survive persistence round-trip")
	}
	edges := kg2.GetEdges("a")
	if len(edges) != 1 {
		t.Fatalf("GetEdges(a) after reload = %d, want 1 (edgesByNode index must be rebuilt on load)", len(edges))
	}
}

// TestKnowledgeGraphStartupPruning verifies the Phase 4.5 fix: a persisted
// graph already over maxConceptNodes gets pruned back down to the cap when
// reloaded, not just on the next write.
func TestKnowledgeGraphStartupPruning(t *testing.T) {
	dir := t.TempDir()

	kg1, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	over := maxConceptNodes + 50
	for i := 0; i < over; i++ {
		id := fmt.Sprintf("concept:word%d", i)
		if err := kg1.AddNode(&core.KGNode{ID: id, Label: fmt.Sprintf("word%d", i), Type: core.KGNodeConcept}); err != nil {
			t.Fatalf("AddNode %d: %v", i, err)
		}
	}
	if n, _ := kg1.Stats(); n != over {
		t.Fatalf("Stats() before reload = %d nodes, want %d (AddNode alone must not prune)", n, over)
	}
	kg1.writer.FlushNow()
	kg1.Shutdown()

	kg2, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("reload NewKnowledgeGraph: %v", err)
	}
	defer kg2.Shutdown()

	if n := kg2.conceptCountLocked(); n > maxConceptNodes {
		t.Errorf("concept count after startup pruning = %d, want <= %d", n, maxConceptNodes)
	}
}

func TestRecordWordRelationsAndConceptRelations(t *testing.T) {
	kg := newTestKG(t)
	if err := kg.RecordWordRelations("authentication requires validation and encryption"); err != nil {
		t.Fatalf("RecordWordRelations: %v", err)
	}

	relsIface := kg.ConceptRelations("authentication")
	rels, ok := relsIface.([]ConceptRelation)
	if !ok {
		t.Fatalf("ConceptRelations returned %T, want []ConceptRelation", relsIface)
	}
	if len(rels) == 0 {
		t.Fatal("expected at least one concept relation for 'authentication'")
	}

	// Recording the same co-occurrence again should increment weight, not
	// duplicate the edge.
	if err := kg.RecordWordRelations("authentication requires validation and encryption"); err != nil {
		t.Fatalf("RecordWordRelations (2nd call): %v", err)
	}
	relsAfter := kg.ConceptRelations("authentication").([]ConceptRelation)
	if len(relsAfter) != len(rels) {
		t.Errorf("expected the same set of related concepts after a repeat, got %d vs %d", len(relsAfter), len(rels))
	}
}

func TestNewKnowledgeGraphUsesGivenDir(t *testing.T) {
	dir := t.TempDir()
	kg, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	defer kg.Shutdown()
	mustAddNode(t, kg, "a", core.KGNodeFile)
	kg.writer.FlushNow()

	if _, err := os.Stat(filepath.Join(dir, "knowledge_graph.json")); err != nil {
		t.Errorf("expected knowledge_graph.json to be written in %s: %v", dir, err)
	}
}
