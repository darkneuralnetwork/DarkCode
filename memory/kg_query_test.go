package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/core"
)

// seedGraph builds file → symbol → package facts with varying confidence.
func seedGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	t.Cleanup(kg.Shutdown)

	nodes := []*core.KGNode{
		{ID: "file:main.go", Label: "main.go", Type: core.KGNodeFile, Confidence: 1.0,
			Properties: map[string]string{"origin": "code_index", "commit": "aaaaaaaaaaaa"}},
		{ID: "file:old.go", Label: "old.go", Type: core.KGNodeFile, Confidence: 1.0,
			Properties: map[string]string{"origin": "code_index", "commit": "bbbbbbbbbbbb"}},
		{ID: "symbol:Serve@main.go", Label: "Serve", Type: core.KGNodeSymbol, Confidence: 0.9,
			Properties: map[string]string{"kind": "function", "language": "go"}},
		{ID: "symbol:Shutdown@main.go", Label: "Shutdown", Type: core.KGNodeSymbol, Confidence: 0.3,
			Properties: map[string]string{"kind": "function"}},
		{ID: "package:net/http", Label: "net/http", Type: core.KGNodePackage, Confidence: 0.6},
	}
	for _, n := range nodes {
		if err := kg.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n.ID, err)
		}
	}
	for _, e := range []*core.KGEdge{
		{From: "file:main.go", To: "symbol:Serve@main.go", Relation: core.KGRelDefines, Weight: 1},
		{From: "file:main.go", To: "symbol:Shutdown@main.go", Relation: core.KGRelDefines, Weight: 1},
		{From: "file:main.go", To: "package:net/http", Relation: core.KGRelImports, Weight: 1},
	} {
		if err := kg.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	return kg
}

func TestSearchFiltersAndRanks(t *testing.T) {
	kg := seedGraph(t)

	got := kg.Search("s", core.KGNodeSymbol, 0)
	if len(got) != 2 {
		t.Fatalf("got %d symbol matches, want 2: %+v", len(got), got)
	}
	// Higher confidence first.
	if got[0].Label != "Serve" {
		t.Errorf("results not ranked by confidence: %+v", got)
	}
	if got[0].Kind != "function" || got[0].Language != "go" {
		t.Errorf("properties not surfaced: %+v", got[0])
	}

	if n := len(kg.Search("", core.KGNodeFile, 0)); n != 2 {
		t.Errorf("empty term with a type filter returned %d, want 2 files", n)
	}
	if n := len(kg.Search("", "", 1)); n != 1 {
		t.Errorf("limit not applied, got %d", n)
	}
	if n := len(kg.Search("nothing-matches-this", "", 0)); n != 0 {
		t.Errorf("expected no matches, got %d", n)
	}
}

func TestNeighborsCoversBothDirections(t *testing.T) {
	kg := seedGraph(t)

	out := kg.Neighbors("file:main.go")
	if len(out) != 3 {
		t.Fatalf("got %d neighbours of main.go, want 3: %+v", len(out), out)
	}

	// A symbol reached from its defining file sees the edge as incoming.
	back := kg.Neighbors("symbol:Serve@main.go")
	if len(back) != 1 || back[0].ID != "file:main.go" {
		t.Fatalf("reverse lookup = %+v, want file:main.go", back)
	}
	if back[0].Relation != "defines (incoming)" {
		t.Errorf("relation = %q, want it marked incoming", back[0].Relation)
	}
}

func TestLowConfidenceExcludesUnscoredNodes(t *testing.T) {
	kg := seedGraph(t)
	if err := kg.AddNode(&core.KGNode{ID: "concept:legacy", Label: "legacy", Type: core.KGNodeConcept}); err != nil {
		t.Fatal(err)
	}

	out := kg.LowConfidence(0.5, 0)
	if len(out) != 1 || out[0].Label != "Shutdown" {
		t.Fatalf("LowConfidence = %+v, want only the 0.3-confidence symbol", out)
	}

	// Weakest first, and the ceiling is respected.
	all := kg.LowConfidence(1.0, 0)
	if len(all) < 2 || all[0].Confidence > all[1].Confidence {
		t.Errorf("results not ordered weakest-first: %+v", all)
	}
}

// Evidence about one node should reach its neighbours with a decaying effect,
// and stop at the hop limit.
func TestPropagateConfidenceDecaysWithDistance(t *testing.T) {
	kg := seedGraph(t)

	before := map[string]float64{}
	for _, id := range []string{"file:main.go", "symbol:Serve@main.go", "package:net/http"} {
		n, _ := kg.GetNode(id)
		before[id] = n.Confidence
	}

	changed := kg.PropagateConfidence("file:main.go", -0.4, 0.5, 1)
	if changed != 4 { // the file plus its three neighbours
		t.Errorf("adjusted %d nodes, want 4", changed)
	}

	root, _ := kg.GetNode("file:main.go")
	neighbour, _ := kg.GetNode("symbol:Serve@main.go")
	rootDrop := before["file:main.go"] - root.Confidence
	neighbourDrop := before["symbol:Serve@main.go"] - neighbour.Confidence

	if rootDrop <= neighbourDrop {
		t.Errorf("root fell %.3f, neighbour %.3f — the effect must decay with distance", rootDrop, neighbourDrop)
	}
	if neighbourDrop <= 0 {
		t.Error("neighbour confidence did not move at all")
	}
}

func TestPropagateConfidenceHopZeroIsLocal(t *testing.T) {
	kg := seedGraph(t)
	if changed := kg.PropagateConfidence("file:main.go", -0.1, 0.5, 0); changed != 1 {
		t.Errorf("hop 0 adjusted %d nodes, want just the target", changed)
	}
	if n := kg.PropagateConfidence("file:main.go", 0, 0.5, 3); n != 0 {
		t.Errorf("a zero delta should be a no-op, adjusted %d", n)
	}
	if n := kg.PropagateConfidence("does-not-exist", -0.1, 0.5, 0); n != 0 {
		t.Errorf("unknown node adjusted %d", n)
	}
}

// Outside a git repository there is no HEAD to compare against, so nothing is
// reported stale rather than everything.
// TestStaleFilesDoesNotNeedGit — REPURPOSED. This used to assert that
// staleness returns nothing outside a git repository, which was true only
// because the answer was computed by comparing a recorded commit to HEAD.
// Staleness is content-based now, so it works in a directory that was never a
// repository at all — and the old assertion would have passed for the wrong
// reason regardless, since the seeded nodes carry no observed content.
func TestStaleFilesDoesNotNeedGit(t *testing.T) {
	kg := seedGraph(t)
	ws := t.TempDir() // deliberately not a repository

	// Nothing observed yet: nothing stale.
	if out := kg.StaleFiles(ws); len(out) != 0 {
		t.Fatalf("unobserved files reported stale: %+v", out)
	}

	// Observe one, then change it on disk. No commits are involved anywhere.
	path := filepath.Join(ws, "tracked.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = kg.AddNode(&core.KGNode{
		ID: "file:tracked.go", Label: "tracked.go", Type: core.KGNodeFile,
		Properties: map[string]string{fileHashProperty: ContentHash("v1")},
		Confidence: 1.0,
	})
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	if out := kg.StaleFiles(ws); len(out) != 1 {
		t.Errorf("StaleFiles = %d entries outside a repo, want 1 — staleness must "+
			"not depend on git", len(out))
	}
}
