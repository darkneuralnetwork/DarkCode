package orchestrator

import "testing"

func TestNextPendingWorkflowTask_PrefersIDFormat(t *testing.T) {
	workflow := "- [x] T1: set up schema\n- [ ] T2: build the API\n- [ ] T3: write tests\n"
	id, line, ok := NextPendingWorkflowTask(workflow)
	if !ok {
		t.Fatal("expected a pending task to be found")
	}
	if id != "T2" || line != "build the API" {
		t.Errorf("got id=%q line=%q, want id=T2 line=%q", id, line, "build the API")
	}
}

func TestNextPendingWorkflowTask_LegacyFallback(t *testing.T) {
	workflow := "## Components\n- [ ] wire up the database connection\n"
	id, line, ok := NextPendingWorkflowTask(workflow)
	if !ok {
		t.Fatal("expected the legacy pending line to be found")
	}
	if id != "" {
		t.Errorf("legacy line should have no ID, got %q", id)
	}
	if line != "wire up the database connection" {
		t.Errorf("line = %q, want %q", line, "wire up the database connection")
	}
}

func TestNextPendingWorkflowTask_NoPendingReturnsFalse(t *testing.T) {
	workflow := "- [x] T1: done\n- [/] T2: running\n"
	if _, _, ok := NextPendingWorkflowTask(workflow); ok {
		t.Error("expected no pending task when all tasks are done/running")
	}
}

func TestResolveTaskGoal_ShortGoalWithPendingTask(t *testing.T) {
	workflow := "- [ ] T4: implement rate limiting\n"
	enriched, ok := resolveTaskGoal("continue", workflow)
	if !ok {
		t.Fatal("expected resolveTaskGoal to enrich a short continuation")
	}
	if !containsAll(enriched, "T4", "implement rate limiting", "continue") {
		t.Errorf("enriched goal missing expected content: %s", enriched)
	}
}

func TestResolveTaskGoal_LongGoalNeverEnriched(t *testing.T) {
	workflow := "- [ ] T1: implement rate limiting\n"
	if _, ok := resolveTaskGoal("please refactor the entire authentication module to use JWT tokens instead", workflow); ok {
		t.Error("a real, self-contained instruction must not be rewritten from the workflow")
	}
}

func TestResolveTaskGoal_ShortGoalNoPendingTaskFallsThrough(t *testing.T) {
	workflow := "- [x] T1: everything already done\n"
	if _, ok := resolveTaskGoal("continue", workflow); ok {
		t.Error("with nothing pending, resolveTaskGoal must not fabricate a task")
	}
}
