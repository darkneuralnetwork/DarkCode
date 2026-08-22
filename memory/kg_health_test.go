package memory

import (
	"strconv"
	"testing"

	"github.com/darkcode/core"
)

// healthGraph builds a small repository:
//
//	core/types.go   defines Config, referenced by api/server.go and cli/main.go
//	api/server.go   defines Serve  (referenced by cli/main.go, and by a test)
//	cli/main.go     defines Orphan (referenced by nobody)
//	api ↔ cli import each other (a cycle)
func healthGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)

	addFile := func(rel string) {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + rel, Label: rel, Type: core.KGNodeFile,
			Confidence: 1, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"core/types.go", "api/server.go", "cli/main.go", "api/server_test.go"} {
		addFile(f)
	}

	addSymbol := func(name, file, kind string, refs int, refFiles ...string) {
		id := "symbol:" + name + "@" + file
		if err := kg.AddNode(&core.KGNode{ID: id, Label: name, Type: core.KGNodeSymbol, Confidence: 1,
			Provenance: file + ":1",
			Properties: map[string]string{"origin": "code_index", "kind": kind, "references": strconv.Itoa(refs)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + file, To: id, Relation: core.KGRelDefines, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		for _, rf := range refFiles {
			if err := kg.AddEdge(&core.KGEdge{From: "file:" + rf, To: id, Relation: core.KGRelReferences, Weight: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	addSymbol("Config", "core/types.go", "struct", 6, "api/server.go", "cli/main.go")
	addSymbol("Serve", "api/server.go", "function", 6, "cli/main.go", "api/server_test.go")
	addSymbol("Orphan", "cli/main.go", "function", 0)

	addImport := func(from, pkg string) {
		id := "package:" + pkg
		if err := kg.AddNode(&core.KGNode{ID: id, Label: pkg, Type: core.KGNodePackage, Confidence: 1}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + from, To: id, Relation: core.KGRelImports, Weight: 1}); err != nil {
			t.Fatal(err)
		}
	}
	addImport("api/server.go", "example.com/m/cli")
	addImport("cli/main.go", "example.com/m/api")
	addImport("api/server.go", "net/http") // external, must not appear in the cycle graph

	return kg
}

func TestBlastRadiusFindsDependents(t *testing.T) {
	kg := healthGraph(t)

	imp := kg.BlastRadius([]string{"core/types.go"}, 1)
	if len(imp.Affected) != 2 {
		t.Fatalf("affected = %v, want api/server.go and cli/main.go", imp.Affected)
	}
	if imp.Severity <= 0 || imp.Severity > 1 {
		t.Errorf("severity %.3f out of range", imp.Severity)
	}
	found := false
	for _, s := range imp.Symbols {
		if s == "Config" {
			found = true
		}
	}
	if !found {
		t.Errorf("symbols = %v, want Config", imp.Symbols)
	}

	// A leaf file nothing references has no radius.
	if leaf := kg.BlastRadius([]string{"api/server_test.go"}, 2); len(leaf.Affected) != 0 {
		t.Errorf("test file radius = %v, want empty", leaf.Affected)
	}
}

// Depth 2 must reach transitively: core → api → (whatever references api).
func TestBlastRadiusHonoursDepth(t *testing.T) {
	kg := healthGraph(t)
	shallow := kg.BlastRadius([]string{"core/types.go"}, 1)
	deep := kg.BlastRadius([]string{"core/types.go"}, 2)
	if len(deep.Affected) < len(shallow.Affected) {
		t.Errorf("deeper search returned fewer files: %v vs %v", deep.Affected, shallow.Affected)
	}
	// The file asked about is never reported as affected by itself.
	for _, f := range deep.Affected {
		if f == "core/types.go" {
			t.Error("the changed file appears in its own blast radius")
		}
	}
}

// A node id works as well as a bare path, since the agent may pass either.
func TestBlastRadiusAcceptsNodeIDs(t *testing.T) {
	kg := healthGraph(t)
	byPath := kg.BlastRadius([]string{"core/types.go"}, 1)
	byID := kg.BlastRadius([]string{"file:core/types.go"}, 1)
	if len(byPath.Affected) != len(byID.Affected) {
		t.Errorf("path form gave %v, id form gave %v", byPath.Affected, byID.Affected)
	}
}

func TestDeadSymbolsSkipsEntryPoints(t *testing.T) {
	kg := healthGraph(t)
	// main and a test function have no in-repo references but are not dead.
	for _, name := range []string{"main", "TestThing"} {
		id := "symbol:" + name + "@cli/main.go"
		if err := kg.AddNode(&core.KGNode{ID: id, Label: name, Type: core.KGNodeSymbol, Confidence: 1,
			Properties: map[string]string{"origin": "code_index", "kind": "function", "references": "0"}}); err != nil {
			t.Fatal(err)
		}
	}

	dead := kg.DeadSymbols()
	if len(dead) != 1 || dead[0].Subject != "Orphan" {
		t.Fatalf("DeadSymbols = %+v, want only Orphan", dead)
	}
}

func TestCyclesDetectsLocalImportCycle(t *testing.T) {
	kg := healthGraph(t)
	cycles := kg.Cycles()
	if len(cycles) == 0 {
		t.Fatal("expected the api ↔ cli cycle to be reported")
	}
	if cycles[0].Kind != "import-cycle" || cycles[0].Detail == "" {
		t.Errorf("malformed finding: %+v", cycles[0])
	}
	// An external import must never be treated as a local package.
	for _, c := range cycles {
		if c.Subject == "net/http" {
			t.Error("external dependency reported as part of a cycle")
		}
	}
}

func TestUntestedHotspotsExcludesTestedSymbols(t *testing.T) {
	kg := healthGraph(t)
	out := kg.UntestedHotspots(0)

	for _, f := range out {
		if f.Subject == "Serve" {
			t.Error("Serve is referenced by api/server_test.go and must not be reported untested")
		}
		if f.Subject == "Orphan" {
			t.Error("a symbol below the fan-in threshold is not a hotspot")
		}
	}
	// Config has two non-test referencing files and no test.
	if len(out) != 1 || out[0].Subject != "Config" {
		t.Fatalf("UntestedHotspots = %+v, want only Config", out)
	}
}

func TestHealthScoreAndRanking(t *testing.T) {
	kg := healthGraph(t)
	rep := kg.Health()

	if rep.Files != 4 || rep.Symbols != 3 {
		t.Errorf("counts = %d files / %d symbols, want 4 / 3", rep.Files, rep.Symbols)
	}
	if rep.Score < 0 || rep.Score > 100 {
		t.Errorf("score %.1f out of range", rep.Score)
	}
	if len(rep.Findings) == 0 {
		t.Fatal("expected findings from a graph with a cycle, dead code and an untested hotspot")
	}
	for i := 1; i < len(rep.Findings); i++ {
		if rep.Findings[i-1].Weight < rep.Findings[i].Weight {
			t.Errorf("findings not ranked worst-first at %d: %+v", i, rep.Findings)
			break
		}
	}
}

// A clean graph should score at or near full marks.
func TestHealthOfCleanGraph(t *testing.T) {
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)
	if rep := kg.Health(); rep.Score != 100 {
		t.Errorf("empty graph scored %.1f, want 100", rep.Score)
	}
}

// A rollback is evidence that the graph's beliefs about the reverted files are
// stale. Confidence must fall for them, and less so for their neighbours.
func TestPropagateConfidenceSoftensRolledBackFiles(t *testing.T) {
	kg := healthGraph(t)

	before := map[string]float64{}
	for _, id := range []string{"file:core/types.go", "symbol:Config@core/types.go", "file:api/server.go"} {
		n, ok := kg.GetNode(id)
		if !ok {
			t.Fatalf("%s missing from the fixture", id)
		}
		before[id] = n.Confidence
	}

	// Same call the kernel makes after a rollback of core/types.go.
	changed := kg.PropagateConfidence("file:core/types.go", -0.2, 0.5, 2)
	if changed == 0 {
		t.Fatal("nothing was softened")
	}

	root, _ := kg.GetNode("file:core/types.go")
	sym, _ := kg.GetNode("symbol:Config@core/types.go")
	if root.Confidence >= before["file:core/types.go"] {
		t.Error("the reverted file's confidence did not fall")
	}
	rootDrop := before["file:core/types.go"] - root.Confidence
	symDrop := before["symbol:Config@core/types.go"] - sym.Confidence
	if symDrop <= 0 {
		t.Error("symbols defined by the reverted file should also be softened")
	}
	if symDrop >= rootDrop {
		t.Errorf("neighbour fell %.3f vs root %.3f — the effect must decay", symDrop, rootDrop)
	}
	// Confidence must stay in range.
	for _, n := range kg.AllNodes() {
		if n.Confidence < 0 || n.Confidence > 1 {
			t.Errorf("%s confidence %.3f out of range", n.ID, n.Confidence)
		}
	}
}
