package orchestrator

import (
	"strings"
	"testing"

	"github.com/darkcode/plan"
)

// A sub-agent that describes an implementation instead of writing it produces
// a confident answer, a completed node and an empty proof set. The run then
// reads as success, because the only thing that would have contradicted it —
// the acceptance summary — rendered as nothing at all.
//
// This is the shape of a real run: the answer said "Implementation provided",
// no file was created, and no line of output disagreed.

func TestNothingVerifiedIsSaidOutLoud(t *testing.T) {
	// A plan the planner returned without acceptance criteria or artifacts.
	g := &plan.Graph{Goal: "add blog post CRUD", Nodes: []*plan.Node{
		{ID: "T4", Name: "blog_post_management"},
	}}

	got := acceptanceSummary(g)
	if got == "" {
		t.Fatal("a run that verified nothing produced no notice, so prose passes as work")
	}
	if !strings.Contains(strings.ToLower(got), "nothing was verified") {
		t.Errorf("the notice does not say what happened: %q", got)
	}
	if !strings.Contains(got, "no acceptance") {
		t.Errorf("the notice does not say why nothing ran: %q", got)
	}
}

// TestUnverifiableCriteriaAreDistinguished. "The planner never said what done
// looks like" and "the criteria were prose" need different fixes, so they read
// differently.
func TestUnverifiableCriteriaAreDistinguished(t *testing.T) {
	prose := &plan.Graph{Nodes: []*plan.Node{
		{ID: "T1", Acceptance: []string{"the code should be clean and idiomatic"}},
	}}
	none := &plan.Graph{Nodes: []*plan.Node{{ID: "T1"}}}

	a, b := acceptanceSummary(prose), acceptanceSummary(none)
	if a == "" || b == "" {
		t.Fatal("one of the unverified cases stayed silent")
	}
	if a == b {
		t.Error("a plan with prose criteria reads the same as one with none, " +
			"though the fix differs")
	}
	if !strings.Contains(a, "prose") {
		t.Errorf("the prose-criteria notice does not name the cause: %q", a)
	}
}

// TestRealChecksStillReportNormally. The notice must not replace the summary
// that appears when work actually was verified.
func TestRealChecksStillReportNormally(t *testing.T) {
	passing := &plan.Graph{Nodes: []*plan.Node{{
		ID: "T1", Acceptance: []string{"tests"},
		Proof: []plan.Proof{{Criterion: "tests", Command: "go test ./...", Passed: true}},
	}}}

	got := acceptanceSummary(passing)
	if strings.Contains(strings.ToLower(got), "nothing was verified") {
		t.Errorf("a verified run was reported as unverified: %q", got)
	}
	if !strings.Contains(got, "all passing") || !strings.Contains(got, "go test") {
		t.Errorf("the passing summary was lost: %q", got)
	}

	failing := &plan.Graph{Nodes: []*plan.Node{{
		ID: "T1", Acceptance: []string{"tests"},
		Proof: []plan.Proof{{Criterion: "tests", Command: "go test ./...", Passed: false}},
	}}}
	if f := acceptanceSummary(failing); !strings.Contains(f, "failing") {
		t.Errorf("a failing check was not reported as failing: %q", f)
	}
}

// TestAMissingArtifactIsEvidence. An artifact path is the cheapest possible
// check on "did the work happen", and it catches exactly the run where an
// agent explained the file instead of creating it.
func TestAMissingArtifactIsEvidence(t *testing.T) {
	k := &Kernel{}
	g := &plan.Graph{Nodes: []*plan.Node{{
		ID: "T4", Artifacts: []string{"src/BlogPostManager.php"},
	}}}

	v := k.verifyContract(t.Context(), g)

	if v.Checked == 0 {
		t.Fatal("an artifact path counted as no check at all")
	}
	if v.Passed {
		t.Error("a plan whose expected file does not exist reported as passing")
	}
	if !strings.Contains(v.Evidence, "BlogPostManager.php") {
		t.Errorf("the evidence does not name the missing file: %q", v.Evidence)
	}
	if v.Proven() {
		t.Error("an unwritten artifact was reported as proven")
	}
}
