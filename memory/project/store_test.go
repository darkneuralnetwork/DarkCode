package project

import (
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// TestBuildContextQuery_CapsContext is the regression test for the one
// injection path in the codebase that had no cap at all: context.md can grow
// up to maxContextBytes (1 MiB, SetContext's own cap) before automatic
// recompression catches up, and BuildContextQuery used to inject every byte
// of it into the query with no further bound — see Context management Phase
// 4 of the context-management unification.
func TestBuildContextQuery_CapsContext(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("demo", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("word ", 100000) // ~500KB, well under maxContextBytes but way over the injection cap
	if err := s.SetContext(p.ID, huge); err != nil {
		t.Fatal(err)
	}

	query := s.BuildContextQuery(p.ID, "do the task")
	if len(query) >= len(huge) {
		t.Fatalf("BuildContextQuery did not cap context: query is %d bytes, source context was %d bytes", len(query), len(huge))
	}
	if !strings.Contains(query, "## Task\ndo the task") {
		t.Errorf("expected the task to survive verbatim, got: %q", query[len(query)-50:])
	}
	if len(query) > maxContextInjectBytes+len("## Project Context\n\n\n## Task\ndo the task")+64 {
		t.Errorf("query (%d bytes) is much larger than maxContextInjectBytes (%d) plus overhead", len(query), maxContextInjectBytes)
	}
}

func TestMarkTaskStatus_FlipsCheckboxOnly(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("demo", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	workflow := "# Workflow\n\n- [ ] T1: set up the database schema\n- [ ] T2: implement the login endpoint\n"
	if err := s.SetWorkflow(p.ID, workflow); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkTaskStatus(p.ID, "T2", TaskDone); err != nil {
		t.Fatalf("MarkTaskStatus: %v", err)
	}

	got, err := s.GetWorkflow(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "- [ ] T1: set up the database schema") {
		t.Errorf("T1 line changed unexpectedly:\n%s", got)
	}
	if !strings.Contains(got, "- [x] T2: implement the login endpoint") {
		t.Errorf("T2 not marked done:\n%s", got)
	}
}

func TestMarkTaskStatus_Running(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("demo", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkflow(p.ID, "- [ ] T1: do the thing\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTaskStatus(p.ID, "T1", TaskRunning); err != nil {
		t.Fatalf("MarkTaskStatus: %v", err)
	}
	got, _ := s.GetWorkflow(p.ID)
	if !strings.Contains(got, "- [/] T1: do the thing") {
		t.Errorf("T1 not marked running:\n%s", got)
	}
}

func TestMarkTaskStatus_UnknownTaskIDIsNoop(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("demo", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	original := "- [ ] T1: do the thing\n"
	if err := s.SetWorkflow(p.ID, original); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTaskStatus(p.ID, "T99", TaskDone); err != nil {
		t.Fatalf("MarkTaskStatus on unknown ID should not error: %v", err)
	}
	got, _ := s.GetWorkflow(p.ID)
	if got != original {
		t.Errorf("workflow changed on unknown task ID: got %q, want unchanged %q", got, original)
	}
}

func TestDefaultSkeletons_UseMatchingTaskID(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("demo", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.EnsurePlanSeeded(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := s.EnsureWorkflowSeeded(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "T1[") {
		t.Errorf("plan skeleton missing a T1 mermaid node:\n%s", plan)
	}
	if !strings.Contains(workflow, "T1:") {
		t.Errorf("workflow skeleton missing a T1 task line:\n%s", workflow)
	}
	// The seeded workflow's task ID must be markable via MarkTaskStatus,
	// proving the two skeletons actually agree on the ID format.
	if err := s.MarkTaskStatus(p.ID, "T1", TaskRunning); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetWorkflow(p.ID)
	if !strings.Contains(got, "- [/] T1:") {
		t.Errorf("seeded T1 task not markable:\n%s", got)
	}
}
