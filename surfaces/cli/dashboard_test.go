package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

// These cover the live activity feed's rendering of event types that
// previously fell through eventIcon/eventMessage's default case (a bare "•"
// bullet and a raw %v dump): file_change, plan_updated, workflow_updated,
// approval, and the task_update "thinking" status. A wrong icon or message
// here means the console's "what is it doing" feed silently degrades back
// to noise for exactly the events (file edits, plan updates) a user most
// wants to see.

func TestEventIconCoversNewTypes(t *testing.T) {
	cases := map[string]string{
		"file_change":      "✎",
		"plan_updated":     "▤",
		"workflow_updated": "⛃",
		"approval":         "⚑",
	}
	for eventType, want := range cases {
		if got := eventIcon(eventType); got != want {
			t.Errorf("eventIcon(%q) = %q, want %q", eventType, got, want)
		}
	}
}

func TestEventIconForDistinguishesThinkingStatus(t *testing.T) {
	thinking := core.UIEvent{Type: core.EventTaskUpdate, Status: "thinking"}
	if got := eventIconFor(thinking); got != "◌" {
		t.Errorf("eventIconFor(thinking) = %q, want ◌", got)
	}

	budget := core.UIEvent{Type: core.EventTaskUpdate, Status: "budget"}
	if got := eventIconFor(budget); got != "⏳" {
		t.Errorf("eventIconFor(budget) = %q, want ⏳", got)
	}

	// A status with no special case falls back to the generic task_update icon.
	observe := core.UIEvent{Type: core.EventTaskUpdate, Status: "observe"}
	if got := eventIconFor(observe); got != "►" {
		t.Errorf("eventIconFor(observe) = %q, want ►", got)
	}
}

func TestEventMessageFileChangeRendersFriendlyLine(t *testing.T) {
	cases := []struct {
		name string
		c    core.Change
		want string
	}{
		{
			name: "modify ok",
			c:    core.Change{Kind: core.ChangeFileModify, Path: "kernel/orchestrator/kernel_execute.go", Success: true},
			want: "✎ modified kernel/orchestrator/kernel_execute.go (ok)",
		},
		{
			name: "create failed",
			c:    core.Change{Kind: core.ChangeFileCreate, Path: "new_file.go", Success: false},
			want: "+ created new_file.go (failed)",
		},
		{
			name: "delete ok",
			c:    core.Change{Kind: core.ChangeFileDelete, Path: "old_file.go", Success: true},
			want: "✗ deleted old_file.go (ok)",
		},
		{
			name: "command falls back to Command when Path is empty",
			c:    core.Change{Kind: core.ChangeCommand, Command: "go test ./...", Success: true},
			want: "$ ran go test ./... (ok)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := core.UIEvent{Type: core.EventFileChange, Content: tc.c}
			if got := eventMessage(e); got != tc.want {
				t.Errorf("eventMessage(file_change %s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestEventMessagePlanAndWorkflowUpdated(t *testing.T) {
	if got := eventMessage(core.UIEvent{Type: core.EventPlanUpdated}); got != "plan updated" {
		t.Errorf("eventMessage(plan_updated) = %q, want %q", got, "plan updated")
	}
	if got := eventMessage(core.UIEvent{Type: core.EventWorkflowUpdated}); got != "workflow updated" {
		t.Errorf("eventMessage(workflow_updated) = %q, want %q", got, "workflow updated")
	}
}

func TestEventMessageApprovalIncludesStatusAndContent(t *testing.T) {
	e := core.UIEvent{Type: core.EventApproval, Status: "denied", Content: "rm -rf /tmp/x"}
	got := eventMessage(e)
	if !strings.Contains(got, "denied") || !strings.Contains(got, "rm -rf /tmp/x") {
		t.Errorf("eventMessage(approval) = %q, want it to mention status and content", got)
	}
}

// TestLiveFeedLineFormat renders the exact "├─ <icon>  <msg>" line format
// runQuery's event handler prints (console.go), for the event types this
// change added coverage for. Not a strict assertion beyond icon+message
// composition — mainly a printed record of what the console now shows, so a
// future reviewer can see the actual output instead of just parsed pieces.
func TestLiveFeedLineFormat(t *testing.T) {
	events := []core.UIEvent{
		{Type: core.EventTaskUpdate, Status: "thinking", Content: "iteration 1/8 — reasoning"},
		{Type: core.EventToolExecution, Tool: "write_file", Status: "completed"},
		{Type: core.EventFileChange, Content: core.Change{
			Kind: core.ChangeFileModify, Path: "kernel/orchestrator/kernel_execute.go", Success: true,
		}},
		{Type: core.EventPlanUpdated},
		{Type: core.EventWorkflowUpdated},
	}
	for _, e := range events {
		icon := eventIconFor(e)
		msg := eventMessage(e)
		line := fmt.Sprintf("  ├─ %s  %s", icon, msg)
		t.Logf("%s", line)
		if icon == "" || msg == "" {
			t.Errorf("empty icon or message for %+v: icon=%q msg=%q", e, icon, msg)
		}
	}
}
