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

func TestSyncWorkspaceKG_NonGoWorkspaceIsNoop(t *testing.T) {
	kg := newTestKG(t)
	stats, err := SyncWorkspaceKG(context.Background(), t.TempDir(), kg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 0 || stats.Symbols != 0 {
		t.Fatalf("expected zero stats for empty workspace, got %+v", stats)
	}
}
