package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// TestLegacyJSONMigrates is the one that matters for existing installs: their
// whole accumulated graph lives in knowledge_graph.json, and losing it would
// silently reset everything the tool has learned.
func TestLegacyJSONMigrates(t *testing.T) {
	dir := t.TempDir()

	legacy := kgData{
		Nodes: []*core.KGNode{
			{ID: "file:main.go", Label: "main.go", Type: core.KGNodeFile, CreatedAt: time.Now().Add(-time.Hour),
				Provenance: "main.go:1", Confidence: 0.9, Properties: map[string]string{"pkg": "main"},
				Vector: []float32{0.25, -0.5, 0.75}},
			{ID: "file:util.go", Label: "util.go", Type: core.KGNodeFile, CreatedAt: time.Now()},
		},
		Edges: []*core.KGEdge{
			{From: "file:main.go", To: "file:util.go", Relation: core.KGRelImports, Weight: 2, CreatedAt: time.Now()},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "knowledge_graph.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	kg, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("NewKnowledgeGraph on a legacy store: %v", err)
	}
	n, e := kg.Stats()
	if n != 2 || e != 1 {
		t.Fatalf("after migration: %d nodes, %d edges; want 2 and 1", n, e)
	}
	kg.Shutdown()

	// Reopen: the data must now come from SQLite, and every field must have
	// survived the round trip — a migration that drops provenance or confidence
	// silently degrades every future graph answer.
	kg2, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer kg2.Shutdown()

	got, ok := kg2.GetNode("file:main.go")
	if !ok {
		t.Fatal("migrated node is missing after reopen")
	}
	if got.Label != "main.go" || got.Type != core.KGNodeFile {
		t.Errorf("identity lost: label=%q type=%q", got.Label, got.Type)
	}
	if got.Provenance != "main.go:1" {
		t.Errorf("provenance lost: %q — graph answers rely on it to skip the LLM safely", got.Provenance)
	}
	if got.Confidence != 0.9 {
		t.Errorf("confidence lost: %v", got.Confidence)
	}
	if got.Properties["pkg"] != "main" {
		t.Errorf("properties lost: %v", got.Properties)
	}
	if len(got.Vector) != 3 || got.Vector[0] != 0.25 || got.Vector[1] != -0.5 || got.Vector[2] != 0.75 {
		t.Errorf("vector lost or corrupted in the BLOB round trip: %v", got.Vector)
	}
	if _, e := kg2.Stats(); e != 1 {
		t.Errorf("edge count after reopen = %d, want 1", e)
	}
}

// TestMigrationRunsOnce — re-importing on every start would resurrect nodes the
// user deleted and undo pruning.
func TestMigrationRunsOnce(t *testing.T) {
	dir := t.TempDir()
	data, _ := json.Marshal(kgData{Nodes: []*core.KGNode{
		{ID: "a", Label: "a", Type: core.KGNodeFile, CreatedAt: time.Now()},
		{ID: "b", Label: "b", Type: core.KGNodeFile, CreatedAt: time.Now()},
	}})
	if err := os.WriteFile(filepath.Join(dir, "knowledge_graph.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	kg, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := kg.RemoveNode("a"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	kg.Shutdown()

	kg2, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer kg2.Shutdown()

	if _, ok := kg2.GetNode("a"); ok {
		t.Error("a deleted node came back; the legacy JSON is being re-imported on every start")
	}
	if _, ok := kg2.GetNode("b"); !ok {
		t.Error("a surviving node was lost")
	}
}

// TestCorruptLegacyJSONDoesNotBlockStartup — the graph is a derived cache. A
// truncated file (killed mid-write by the old writer) must not stop the process
// from starting; re-indexing rebuilds it.
func TestCorruptLegacyJSONDoesNotBlockStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "knowledge_graph.json"), []byte(`{"nodes":[{"id":"a"`), 0o644); err != nil {
		t.Fatal(err)
	}
	kg, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatalf("a corrupt legacy file must not fail startup: %v", err)
	}
	defer kg.Shutdown()
	if n, _ := kg.Stats(); n != 0 {
		t.Errorf("expected an empty graph from a corrupt file, got %d nodes", n)
	}
}

// TestVectorBlobRoundTrip pins the encoding directly, including the edges of
// the float32 range that a naive conversion would mangle.
func TestVectorBlobRoundTrip(t *testing.T) {
	cases := [][]float32{
		nil,
		{},
		{0},
		{1, -1, 0.5, -0.5},
		{3.4028235e38, -3.4028235e38, 1e-45},
	}
	for _, in := range cases {
		out := decodeVector(encodeVector(in))
		if len(in) == 0 {
			if len(out) != 0 {
				t.Errorf("empty vector round-tripped to %v", out)
			}
			continue
		}
		if len(out) != len(in) {
			t.Fatalf("length changed: %d -> %d", len(in), len(out))
		}
		for i := range in {
			if out[i] != in[i] {
				t.Errorf("element %d: %v -> %v", i, in[i], out[i])
			}
		}
	}
}

// TestNodeUpdateWritesOneRow — the whole point of the change. An update must
// not grow the store, and it must be visible immediately.
func TestNodeUpdateWritesOneRow(t *testing.T) {
	dir := t.TempDir()
	kg, err := NewKnowledgeGraph(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer kg.Shutdown()

	for i := 0; i < 20; i++ {
		if err := kg.AddNode(&core.KGNode{ID: "same", Label: "v", Type: core.KGNodeFile, Confidence: float64(i) / 20}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := kg.Stats(); n != 1 {
		t.Errorf("20 updates to one id produced %d nodes", n)
	}

	probe, err := openKGStore(filepath.Join(dir, "knowledge_graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	nodes, _, err := probe.Count()
	if err != nil {
		t.Fatal(err)
	}
	if nodes != 1 {
		t.Errorf("store holds %d rows for one node id; upsert is appending instead of replacing", nodes)
	}
}
