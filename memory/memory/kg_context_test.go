package memory

import (
	"strconv"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

// contextGraph indexes a few files with distinct vocabularies so relevance is
// unambiguous.
func contextGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)

	addFile := func(rel string, conf float64) {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + rel, Label: rel, Type: core.KGNodeFile,
			Confidence: conf, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	addSymbol := func(name, file, kind string, refs int) {
		id := "symbol:" + name + "@" + file
		if err := kg.AddNode(&core.KGNode{ID: id, Label: name, Type: core.KGNodeSymbol, Confidence: 1,
			Properties: map[string]string{"origin": "code_index", "kind": kind, "references": strconv.Itoa(refs)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + file, To: id, Relation: core.KGRelDefines, Weight: 1}); err != nil {
			t.Fatal(err)
		}
	}
	addImport := func(file, pkg string) {
		id := "package:" + pkg
		if err := kg.AddNode(&core.KGNode{ID: id, Label: pkg, Type: core.KGNodePackage, Confidence: 1}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + file, To: id, Relation: core.KGRelImports, Weight: 1}); err != nil {
			t.Fatal(err)
		}
	}

	addFile("permission/gate.go", 1)
	addSymbol("Gate", "permission/gate.go", "struct", 40)
	addSymbol("Check", "permission/gate.go", "function", 12)
	addImport("permission/gate.go", "core")

	addFile("billing/invoice.go", 1)
	addSymbol("Invoice", "billing/invoice.go", "struct", 3)

	addFile("permission/gate_test.go", 1)
	addSymbol("TestGateCheck", "permission/gate_test.go", "function", 0)

	addFile("legacy/old.go", 0.4) // a low-confidence belief
	addSymbol("Legacy", "legacy/old.go", "function", 1)

	return kg
}

func TestStructuralViewRanksByRelevance(t *testing.T) {
	kg := contextGraph(t)
	view, sv := kg.StructuralView("fix the permission gate check", 800)
	if view == "" {
		t.Fatal("no view produced")
	}
	if !strings.Contains(view, "permission/gate.go") {
		t.Errorf("the relevant file is missing:\n%s", view)
	}
	if strings.Contains(view, "billing/invoice.go") {
		t.Errorf("an unrelated file was included:\n%s", view)
	}
	if sv.Files == 0 || sv.ViewTokens == 0 {
		t.Errorf("savings not measured: %+v", sv)
	}
	// Fan-in is the signal that tells the model what a change would cost.
	if !strings.Contains(view, "<-") {
		t.Errorf("fan-in not rendered:\n%s", view)
	}
}

// A test file should not crowd out the production code it tests.
func TestStructuralViewDownweightsTestFiles(t *testing.T) {
	kg := contextGraph(t)
	view, _ := kg.StructuralView("permission gate", 800)

	src := strings.Index(view, "\npermission/gate.go")
	test := strings.Index(view, "\npermission/gate_test.go")
	if src < 0 {
		t.Fatalf("source file missing:\n%s", view)
	}
	if test >= 0 && test < src {
		t.Errorf("test file outranked the source it tests:\n%s", view)
	}
}

// Low confidence has to be visible, or the model treats a shaky belief as
// solid.
func TestStructuralViewShowsLowConfidence(t *testing.T) {
	kg := contextGraph(t)
	view, _ := kg.StructuralView("legacy", 800)
	if !strings.Contains(view, "?0.4") {
		t.Errorf("confidence below 1 not surfaced:\n%s", view)
	}
}

// The budget is the whole point: an unbounded skeleton is just another way to
// blow the context window.
func TestStructuralViewRespectsBudget(t *testing.T) {
	kg := contextGraph(t)
	for _, budget := range []int{60, 120, 400} {
		view, _ := kg.StructuralView("permission gate check legacy billing", budget)
		if got := len(view) / charsPerToken; got > budget+40 {
			t.Errorf("budget %d exceeded: view is ~%d tokens", budget, got)
		}
	}
}

func TestStructuralViewEmptyCases(t *testing.T) {
	kg := contextGraph(t)
	if view, sv := kg.StructuralView("", 800); view != "" || sv.Files != 0 {
		t.Error("an empty goal should produce no view")
	}
	if view, _ := kg.StructuralView("quantum entanglement of waterfowl", 800); view != "" {
		t.Errorf("an unmatched goal should produce no view, got:\n%s", view)
	}
}

func TestSavingsRatio(t *testing.T) {
	if got := (Savings{SourceTokens: 8000, ViewTokens: 400}).Ratio(); got != 20 {
		t.Errorf("Ratio = %v, want 20", got)
	}
	if got := (Savings{}).Ratio(); got != 0 {
		t.Errorf("empty savings should be 0, got %v", got)
	}
}

// camelCase has to split, or a goal saying "stm" never matches STMAdd.
func TestKeywordsOfSplitsCamelCase(t *testing.T) {
	got := keywordsOf("update STMAdd and parseConfig in HTTPServer")
	for _, want := range []string{"stmadd", "stm", "add", "parseconfig", "parse", "config", "update", "http", "server"} {
		if !got[want] {
			t.Errorf("keyword %q missing from %v", want, got)
		}
	}
	// Stopwords and short tokens carry no signal.
	if got["and"] {
		t.Error("stopword retained")
	}
}

func TestShortKind(t *testing.T) {
	for kind, want := range map[string]string{
		"function": "fn", "method": "m", "struct": "st",
		"interface": "if", "type": "ty", "": "s",
	} {
		if got := shortKind(kind); got != want {
			t.Errorf("shortKind(%q) = %q, want %q", kind, got, want)
		}
	}
}
