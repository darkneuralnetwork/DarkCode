package deterministic

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
)

// writeTestWorkspace lays down a two-file Go mini-project: lib.go defines
// Greet + Config, main.go references Greet and imports fmt.
func writeTestWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lib := `package main

// Greet returns a greeting.
func Greet(name string) string { return "hi " + name }

type Config struct{ Debug bool }
`
	main := `package main

import "fmt"

func main() { fmt.Println(Greet("dark")) }
`
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte(lib), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestKG(t *testing.T) *memory.KnowledgeGraph {
	t.Helper()
	kg, err := memory.NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	t.Cleanup(kg.Shutdown)
	return kg
}

func TestSyncWorkspaceKG_TypedFactsWithProvenance(t *testing.T) {
	dir := writeTestWorkspace(t)
	kg := newTestKG(t)

	stats, err := SyncWorkspaceKG(context.Background(), dir, kg)
	if err != nil {
		t.Fatalf("SyncWorkspaceKG: %v", err)
	}
	if stats.Symbols < 3 { // Greet, Config, main
		t.Fatalf("expected >=3 symbols, got %+v", stats)
	}

	// The Greet symbol node must exist with file:line provenance.
	node, ok := kg.GetNode("symbol:Greet@lib.go")
	if !ok {
		t.Fatal("symbol:Greet@lib.go not found in KG")
	}
	if node.Type != core.KGNodeSymbol {
		t.Fatalf("wrong node type: %s", node.Type)
	}
	if node.Provenance != "lib.go:4" {
		t.Fatalf("wrong provenance: %q", node.Provenance)
	}
	if node.Properties["kind"] != "function" {
		t.Fatalf("wrong kind: %q", node.Properties["kind"])
	}
	// Greet is referenced from main.go (1 other file).
	if node.Properties["references"] != "1" {
		t.Fatalf("wrong reference count: %q", node.Properties["references"])
	}

	// main.go must import fmt (file → package edge).
	foundImport := false
	for _, e := range kg.GetEdges("file:main.go") {
		if e.Relation == core.KGRelImports && e.To == "package:fmt" {
			foundImport = true
		}
	}
	if !foundImport {
		t.Fatal("missing imports edge file:main.go → package:fmt")
	}

	// defines edge: lib.go → Greet.
	foundDefines := false
	for _, e := range kg.GetEdges("file:lib.go") {
		if e.Relation == core.KGRelDefines && e.To == "symbol:Greet@lib.go" {
			foundDefines = true
		}
	}
	if !foundDefines {
		t.Fatal("missing defines edge file:lib.go → symbol:Greet@lib.go")
	}
}

func TestSyncWorkspaceKG_Idempotent(t *testing.T) {
	dir := writeTestWorkspace(t)
	kg := newTestKG(t)

	if _, err := SyncWorkspaceKG(context.Background(), dir, kg); err != nil {
		t.Fatal(err)
	}
	n1, e1 := kg.Stats()
	if _, err := SyncWorkspaceKG(context.Background(), dir, kg); err != nil {
		t.Fatal(err)
	}
	n2, e2 := kg.Stats()
	if n1 != n2 || e1 != e2 {
		t.Fatalf("re-sync grew the graph: nodes %d→%d, edges %d→%d", n1, n2, e1, e2)
	}
}

func TestSyncWorkspaceKG_EmptyWorkspaceIsNoop(t *testing.T) {
	kg := newTestKG(t)
	stats, err := SyncWorkspaceKG(context.Background(), t.TempDir(), kg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 0 || stats.Symbols != 0 {
		t.Fatalf("expected zero stats for empty workspace, got %+v", stats)
	}
}

// A workspace with no Go files must still be indexed — the early return that
// used to short-circuit here left polyglot repos with an empty graph.
func TestSyncWorkspaceKGIndexesNonGoLanguages(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"web/app.ts":   "import { Logger } from \"./log\";\nexport class App extends Base {}\n",
		"svc/main.py":  "import os\n\nclass Service:\n    def run(self):\n        pass\n",
		"core/lib.rs":  "use std::fmt;\npub struct Engine { id: u32 }\n",
		"api/Api.java": "import java.util.List;\npublic class Api {}\n",
	}
	for rel, src := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	kg := newTestKG(t)
	stats, err := SyncWorkspaceKG(context.Background(), root, kg)
	if err != nil {
		t.Fatalf("SyncWorkspaceKG: %v", err)
	}
	if stats.Files != 4 {
		t.Errorf("indexed %d files, want 4", stats.Files)
	}
	if stats.Symbols < 4 {
		t.Errorf("found %d symbols, want at least one per file", stats.Symbols)
	}

	// Symbols from every language land under the same node type and ID scheme.
	want := map[string]bool{"App": false, "Service": false, "Engine": false, "Api": false}
	for _, n := range kg.FindByType(core.KGNodeSymbol) {
		if _, tracked := want[n.Label]; tracked {
			want[n.Label] = true
			if n.Properties["language"] == "" {
				t.Errorf("symbol %s has no language property", n.Label)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("symbol %s missing from the graph", name)
		}
	}
}
