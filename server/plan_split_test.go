package server

import (
	"strings"
	"testing"
)

func TestSplitPlanWorkflow(t *testing.T) {
	// Explicit delimiter.
	plan, wf := splitPlanWorkflow("# Plan\nsome plan\n===WORKFLOW===\n- [ ] T1: do it")
	if !strings.Contains(plan, "some plan") || strings.Contains(plan, "T1") {
		t.Errorf("plan section wrong: %q", plan)
	}
	if !strings.Contains(wf, "T1: do it") {
		t.Errorf("workflow section wrong: %q", wf)
	}

	// No delimiter, checkboxes under a heading → split at the heading.
	plan, wf = splitPlanWorkflow("# Plan\narchitecture stuff\n## Tasks\n- [ ] T1: a\n- [ ] T2: b")
	if strings.Contains(plan, "T1") || !strings.Contains(plan, "architecture stuff") {
		t.Errorf("heuristic plan split wrong: %q", plan)
	}
	if !strings.Contains(wf, "T1: a") || !strings.Contains(wf, "T2: b") {
		t.Errorf("heuristic workflow split wrong: %q", wf)
	}

	// Plan only (no checkboxes) → everything is plan, workflow empty.
	plan, wf = splitPlanWorkflow("# Plan\njust a plan, no tasks")
	if wf != "" || !strings.Contains(plan, "just a plan") {
		t.Errorf("plan-only case wrong: plan=%q wf=%q", plan, wf)
	}
}
