package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// policyGraph: http → store → core, and store has partial test coverage.
func policyGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg := patternGraph(t,
		"store/a.go", "store/a_test.go", "store/b.go", "store/c.go",
		"http/h.go", "core/c.go")
	addImport(t, kg, "http/h.go", "example.com/m/store")
	addImport(t, kg, "store/a.go", "example.com/m/core")
	return kg
}

// The governance case: the rule says it must not, so the day it does is a
// failure rather than a pattern that quietly stopped holding.
func TestForbidImportCatchesTheEdge(t *testing.T) {
	kg := policyGraph(t)

	// This one holds — store does not import http.
	clean := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "storage stays below transport", Kind: "forbid-import", From: "store", To: "http"},
	}})
	if len(clean) != 0 {
		t.Errorf("a rule that holds reported %d breaches: %+v", len(clean), clean)
	}

	// This one does not — http does import store.
	breached := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "transport stays below storage", Kind: "forbid-import", From: "http", To: "store",
			Why: "we decided the dependency runs the other way"},
	}})
	if len(breached) != 1 {
		t.Fatalf("got %d breaches, want 1: %+v", len(breached), breached)
	}
	if breached[0].Subject != "http" {
		t.Errorf("subject = %q, want the offending package", breached[0].Subject)
	}
	if !strings.Contains(FormatBreaches(breached), "we decided") {
		t.Error("the rule's reasoning should appear in the report")
	}
}

func TestForbidImportSupportsPrefixPatterns(t *testing.T) {
	kg := policyGraph(t)
	got := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "nothing reaches store", Kind: "forbid-import", From: "*", To: "sto*"},
	}})
	if len(got) != 1 || got[0].Subject != "http" {
		t.Errorf("prefix matching missed the edge: %+v", got)
	}
}

func TestRequireTestsMeasuresCoverage(t *testing.T) {
	kg := policyGraph(t)

	// store is 1/3 tested, so a 100% requirement fails.
	strict := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "everything tested", Kind: "require-tests", Package: "store", Threshold: 1},
	}})
	if len(strict) != 1 {
		t.Fatalf("got %d breaches, want 1: %+v", len(strict), strict)
	}
	if !strings.Contains(strict[0].Detail, "1 of 3") {
		t.Errorf("detail should show the actual numbers: %q", strict[0].Detail)
	}

	// A 30% requirement is met.
	loose := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "some tests", Kind: "require-tests", Package: "store", Threshold: 0.3},
	}})
	if len(loose) != 0 {
		t.Errorf("a met requirement reported a breach: %+v", loose)
	}
}

// An unstated threshold has to mean something definite.
func TestRequireTestsDefaultsToEverything(t *testing.T) {
	kg := policyGraph(t)
	got := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "tests", Kind: "require-tests", Package: "store"},
	}})
	if len(got) != 1 {
		t.Errorf("an unstated threshold should require all files tested: %+v", got)
	}
}

func TestCouplingAndCycleCeilings(t *testing.T) {
	kg := policyGraph(t)

	if got := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "keep it loose", Kind: "max-coupling", Threshold: 0.1},
	}}); len(got) != 1 {
		t.Errorf("a coupling ceiling below the actual value should fail: %+v", got)
	}
	if got := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "generous", Kind: "max-coupling", Threshold: 99},
	}}); len(got) != 0 {
		t.Errorf("a generous ceiling should hold: %+v", got)
	}
	// This graph is acyclic, so a zero-cycle rule holds.
	if got := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "no cycles", Kind: "max-cycles", Threshold: 0},
	}}); len(got) != 0 {
		t.Errorf("an acyclic graph broke a no-cycles rule: %+v", got)
	}
}

// One impossible rule must not hide the others.
func TestRulesAreEvaluatedIndependently(t *testing.T) {
	kg := policyGraph(t)
	got := kg.CheckPolicy(Policy{Rules: []Rule{
		{Name: "a", Kind: "forbid-import", From: "nonexistent", To: "alsonot"},
		{Name: "b", Kind: "forbid-import", From: "http", To: "store"},
	}})
	if len(got) != 1 || got[0].Rule.Name != "b" {
		t.Errorf("the real breach was lost: %+v", got)
	}
}

// --- loading ---

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPolicy(t *testing.T) {
	p, err := LoadPolicy(writePolicy(t, `{"rules":[
		{"name":"layering","kind":"forbid-import","from":"store","to":"http","why":"one direction"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 1 || p.Rules[0].Why != "one direction" {
		t.Errorf("policy did not round-trip: %+v", p)
	}
}

// Most repositories have no policy, and that is a valid state.
func TestMissingPolicyIsEmptyNotAnError(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Errorf("a missing policy file was an error: %v", err)
	}
	if len(p.Rules) != 0 {
		t.Errorf("a missing file produced rules: %+v", p)
	}
}

// A rule that cannot mean anything must be rejected at load, not silently
// pass every check afterwards.
func TestInvalidRulesAreRejectedAtLoad(t *testing.T) {
	for name, body := range map[string]string{
		"no name":           `{"rules":[{"kind":"max-cycles","threshold":0}]}`,
		"unknown kind":      `{"rules":[{"name":"x","kind":"teleport"}]}`,
		"forbid needs ends": `{"rules":[{"name":"x","kind":"forbid-import","from":"a"}]}`,
		"bad fraction":      `{"rules":[{"name":"x","kind":"require-tests","threshold":7}]}`,
		"malformed json":    `{"rules":[`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPolicy(writePolicy(t, body)); err == nil {
				t.Error("an invalid policy loaded cleanly")
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"sto*", "store", true},
		{"sto*", "storage/x", true},
		{"sto*", "http", false},
		{"store", "store", true},
		{"store", "store/sub", false},
	} {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestFormatBreachesReportsACleanPolicy(t *testing.T) {
	if got := FormatBreaches(nil); !strings.Contains(got, "no violations") {
		t.Errorf("a clean policy should say so: %q", got)
	}
}
