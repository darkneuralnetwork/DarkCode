package server

import (
	"strings"
	"testing"

	"github.com/darkcode/core"
)

func TestNeedsPlanAmend_ColdStartAlwaysAmends(t *testing.T) {
	if !needsPlanAmend("continue", nil, true) {
		t.Error("a cold start (no STM) must always amend, even for a short message")
	}
}

func TestNeedsPlanAmend_ShortContinuationSkipsAmend(t *testing.T) {
	stm := []core.Message{
		{Role: core.RoleUser, Content: "build the login page"},
		{Role: core.RoleAssistant, Content: "done"},
		{Role: core.RoleUser, Content: "continue"},
	}
	if needsPlanAmend("continue", stm, true) {
		t.Error("a short continuation after a real prior turn should skip the amend")
	}
}

func TestNeedsPlanAmend_LongInstructionAfterConversationAmends(t *testing.T) {
	stm := []core.Message{
		{Role: core.RoleUser, Content: "build the login page"},
		{Role: core.RoleAssistant, Content: "done"},
		{Role: core.RoleUser, Content: "now also add rate limiting to the signup endpoint"},
	}
	if !needsPlanAmend("now also add rate limiting to the signup endpoint", stm, true) {
		t.Error("a real new instruction (even mid-conversation) must trigger an amend")
	}
}

func TestNeedsPlanAmend_ReadOnlyQuestionSkipsAmend(t *testing.T) {
	// A cold-start question can't change the plan → skip the 2 amend calls
	// when SkipAuxForReadOnly is on…
	if needsPlanAmend("what does the auth middleware do?", nil, true) {
		t.Error("a read-only question should skip the amend when skipReadOnly is on")
	}
	// …but honor it as an amend when the skip is disabled (opt-out preserved).
	if !needsPlanAmend("what does the auth middleware do?", nil, false) {
		t.Error("with skipReadOnly off, even a question should amend (no behavior change)")
	}
	// A concrete instruction still amends regardless of the flag.
	if !needsPlanAmend("add a rate limiter to the login route", nil, true) {
		t.Error("a concrete instruction must always amend")
	}
}

func TestParseWorkflowTaskStatuses(t *testing.T) {
	workflow := "# Workflow\n\n- [x] T1: set up schema\n- [/] T2: build the API\n- [ ] T3: write tests\n"
	got := parseWorkflowTaskStatuses(workflow)
	want := map[string]string{"T1": "done", "T2": "running", "T3": "pending"}
	if len(got) != len(want) {
		t.Fatalf("parseWorkflowTaskStatuses = %v, want %v", got, want)
	}
	for id, status := range want {
		if got[id] != status {
			t.Errorf("status[%s] = %q, want %q", id, got[id], status)
		}
	}
}

func TestParseWorkflowTaskStatuses_NoTaskLinesReturnsEmpty(t *testing.T) {
	got := parseWorkflowTaskStatuses("# Workflow\n\nJust prose, no checklist.\n")
	if len(got) != 0 {
		t.Errorf("expected no statuses, got %v", got)
	}
}

func TestInjectNodeStatus_StampsClassesOntoMermaidFence(t *testing.T) {
	plan := "# Plan\n\n## Architecture\n```mermaid\ngraph TD\n  T1[Schema]\n  T2[API]\n```\n"
	workflow := "- [x] T1: set up schema\n- [/] T2: build the API\n"

	got := injectNodeStatus(plan, workflow)

	if !strings.Contains(got, "class T1 done") {
		t.Errorf("missing 'class T1 done':\n%s", got)
	}
	if !strings.Contains(got, "class T2 running") {
		t.Errorf("missing 'class T2 running':\n%s", got)
	}
	if !strings.Contains(got, "classDef done") || !strings.Contains(got, "classDef running") || !strings.Contains(got, "classDef pending") {
		t.Errorf("missing classDef declarations:\n%s", got)
	}
	// The original graph body must survive untouched.
	if !strings.Contains(got, "T1[Schema]") || !strings.Contains(got, "T2[API]") {
		t.Errorf("original mermaid body was altered:\n%s", got)
	}
}

func TestInjectNodeStatus_NoMermaidFenceIsNoop(t *testing.T) {
	plan := "# Plan\n\nJust prose, no diagram.\n"
	workflow := "- [x] T1: set up schema\n"
	got := injectNodeStatus(plan, workflow)
	if got != plan {
		t.Errorf("expected no-op when plan has no mermaid fence, got:\n%s", got)
	}
}

func TestInjectNodeStatus_NoWorkflowTasksIsNoop(t *testing.T) {
	plan := "# Plan\n```mermaid\ngraph TD\n  T1[Schema]\n```\n"
	got := injectNodeStatus(plan, "no checklist here")
	if got != plan {
		t.Errorf("expected no-op when workflow has no task lines, got:\n%s", got)
	}
}
