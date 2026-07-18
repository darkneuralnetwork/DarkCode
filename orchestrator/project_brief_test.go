package orchestrator

import (
	"strings"
	"testing"
)

// TestInjectProjectContext_BriefFirst verifies the injection mechanics Phase 4
// relies on: a routine turn passes an empty plan (the brief rides in the query)
// so only the workflow is injected, while a planning/amend turn passes the full
// plan and gets both. This keeps routine project turns small.
func TestInjectProjectContext_BriefFirst(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	// Routine turn: empty plan + a workflow.
	k.SetProjectContext("", "- [ ] T1: build the parser")
	got := k.injectProjectContext("do the next task")
	if strings.Contains(got, "Implementation Plan & Architecture") {
		t.Error("routine turn (empty plan) should NOT inject the full plan section")
	}
	if !strings.Contains(got, "Task Workflow") || !strings.Contains(got, "T1: build the parser") {
		t.Error("routine turn should still inject the task workflow for continuity")
	}
	k.ClearProjectContext()

	// Amend/planning turn: full plan present → both sections injected.
	k.SetProjectContext("Architecture: layered. Constraint: no cgo.", "- [ ] T1: build the parser")
	got = k.injectProjectContext("re-plan the architecture")
	if !strings.Contains(got, "Implementation Plan & Architecture") || !strings.Contains(got, "no cgo") {
		t.Error("amend turn should inject the full plan")
	}
	k.ClearProjectContext()

	// No project context at all → goal returned unchanged.
	if got := k.injectProjectContext("just a question"); got != "just a question" {
		t.Errorf("no project context should return the goal unchanged, got %q", got)
	}
}
