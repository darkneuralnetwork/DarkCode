package memory

import (
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// adjGraph indexes a repository whose facts are known exactly:
//
//	permission/gate.go defines Gate and Check
//	memory/store.go    defines Store
//	permission imports core
func adjGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)

	file := func(rel string) {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + rel, Label: rel, Type: core.KGNodeFile,
			Confidence: 1, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	sym := func(name, rel string) {
		if err := kg.AddNode(&core.KGNode{
			ID: "symbol:" + name + "@" + rel, Label: name, Type: core.KGNodeSymbol,
			Confidence: 1, Provenance: rel + ":10",
			Properties: map[string]string{"origin": "code_index", "kind": "function", "references": "1"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	imp := func(from, pkg string) {
		id := "package:" + pkg
		if err := kg.AddNode(&core.KGNode{ID: id, Label: pkg, Type: core.KGNodePackage, Confidence: 1}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + from, To: id, Relation: core.KGRelImports, Weight: 1}); err != nil {
			t.Fatal(err)
		}
	}

	file("permission/gate.go")
	file("memory/store.go")
	file("core/types.go")
	sym("Gate", "permission/gate.go")
	sym("Check", "permission/gate.go")
	sym("Store", "memory/store.go")
	imp("permission/gate.go", "example.com/m/core")
	return kg
}

func TestAdjudicateVerifiesTrueClaims(t *testing.T) {
	kg := adjGraph(t)
	s := kg.Adjudicate("Gate is defined in permission/gate.go and the Check function handles approval.")
	if s.Checked < 2 {
		t.Fatalf("expected at least two checkable claims, got %+v", s)
	}
	if s.Verified != s.Checked {
		t.Errorf("true claims were not verified: %+v", s.Checks)
	}
	if s.Score() != 1 {
		t.Errorf("Score = %v, want 1", s.Score())
	}
}

// The failure this exists to catch: a model inventing a symbol.
func TestAdjudicateCatchesHallucinatedSymbol(t *testing.T) {
	kg := adjGraph(t)
	s := kg.Adjudicate("Use the FrobnicateWidget function to reset the gate.")
	if s.Checked == 0 {
		t.Fatal("the existence claim was not checked")
	}
	if s.Verified != 0 {
		t.Errorf("an invented symbol was accepted: %+v", s.Checks)
	}
	if len(s.Contradicted()) == 0 {
		t.Error("the contradiction should be reportable")
	}
}

func TestAdjudicateCatchesWrongLocation(t *testing.T) {
	kg := adjGraph(t)
	s := kg.Adjudicate("Gate is defined in memory/store.go.")
	if s.Verified != 0 || s.Checked != 1 {
		t.Fatalf("a wrong location should fail exactly one claim: %+v", s.Checks)
	}
	if !strings.Contains(s.Checks[0].Detail, "permission/gate.go") {
		t.Errorf("the detail should say where it actually is: %q", s.Checks[0].Detail)
	}
}

func TestAdjudicateChecksDependencies(t *testing.T) {
	kg := adjGraph(t)
	if s := kg.Adjudicate("package permission imports core"); s.Verified != 1 {
		t.Errorf("a true dependency was not verified: %+v", s.Checks)
	}
	if s := kg.Adjudicate("package core imports permission"); s.Checked == 0 || s.Verified != 0 {
		t.Errorf("a false dependency was not caught: %+v", s.Checks)
	}
}

// Prose that asserts nothing checkable must neither help nor hurt.
func TestAdjudicateIgnoresUncheckableProse(t *testing.T) {
	kg := adjGraph(t)
	s := kg.Adjudicate("I would start with the simplest approach and iterate from there.")
	if s.Checked != 0 {
		t.Errorf("ordinary prose produced claims: %+v", s.Checks)
	}
	if s.Score() != 0.5 {
		t.Errorf("Score = %v, want the neutral 0.5", s.Score())
	}
}

// Without an index there is nothing to adjudicate against, and a confident
// score computed from nothing is worse than none.
func TestAdjudicateWithoutIndexIsNeutral(t *testing.T) {
	empty, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(empty.Shutdown)

	s := empty.Adjudicate("Gate is defined in permission/gate.go")
	if s.Checked != 0 {
		t.Errorf("an unindexed graph should judge nothing: %+v", s)
	}
}

// The headline behaviour: the better-supported candidate wins.
func TestAdjudicateCandidatesPrefersTheTruthfulAnswer(t *testing.T) {
	kg := adjGraph(t)
	candidates := []string{
		"Gate is defined in memory/store.go",    // wrong
		"Gate is defined in permission/gate.go", // right
	}
	best, supports := kg.AdjudicateCandidates(candidates)
	if best != 1 {
		t.Errorf("best = %d, want the truthful candidate (1); scores %v / %v",
			best, supports[0].Score(), supports[1].Score())
	}
}

// A tie must keep the first candidate, which is the synthesis.
func TestAdjudicateCandidatesKeepsFirstOnTie(t *testing.T) {
	kg := adjGraph(t)
	best, _ := kg.AdjudicateCandidates([]string{
		"Gate is defined in permission/gate.go",
		"Check is defined in permission/gate.go",
	})
	if best != 0 {
		t.Errorf("best = %d, want 0 — a tie should not churn the ordering", best)
	}
}

func TestAdjudicateCandidatesEmpty(t *testing.T) {
	kg := adjGraph(t)
	if best, supports := kg.AdjudicateCandidates(nil); best != -1 || supports != nil {
		t.Errorf("empty input should report no winner, got %d / %v", best, supports)
	}
}

// A qualified name refers to the same symbol the graph indexed bare.
func TestBareName(t *testing.T) {
	for in, want := range map[string]string{
		"permission.Gate": "Gate", "Gate": "Gate",
		"a.b.C": "C", "trailing.": "trailing.",
	} {
		if got := bareName(in); got != want {
			t.Errorf("bareName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A claim may name a bare file or a partial path.
func TestMatchesAnyFile(t *testing.T) {
	files := []string{"permission/gate.go"}
	for _, claimed := range []string{"permission/gate.go", "gate.go"} {
		if !matchesAnyFile(files, claimed) {
			t.Errorf("%q should match %v", claimed, files)
		}
	}
	if matchesAnyFile(files, "memory/gate.go") {
		t.Error("a different directory must not match")
	}
}

// Repeating a claim must not inflate the score.
func TestAdjudicateCountsEachClaimOnce(t *testing.T) {
	kg := adjGraph(t)
	once := kg.Adjudicate("Gate is defined in permission/gate.go")
	twice := kg.Adjudicate("Gate is defined in permission/gate.go. Again: Gate is defined in permission/gate.go")
	if twice.Checked != once.Checked {
		t.Errorf("a repeated claim was counted %d times, want %d", twice.Checked, once.Checked)
	}
}
