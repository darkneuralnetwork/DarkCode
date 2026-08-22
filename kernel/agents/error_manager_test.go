package agents

// error_manager_test.go — ErrorManager had no test coverage anywhere before
// this (it was relocated here from kernel/orchestrator, where it also had
// none). It's the fix for Gemini's thought_signature/INVALID_ARGUMENT 400,
// which very likely explains the QA audit's Finding 5 (a /loop-mode Gemini
// 400 with no equivalent protection in kernel/loop) — worth locking down
// properly now that it's being relied on by a second consumer.

import (
	"errors"
	"testing"

	"github.com/darkcode/infra/core"
)

func TestErrorManagerNilErrorIsNoOp(t *testing.T) {
	em := NewErrorManager()
	history := []core.Message{{Role: core.RoleUser, Content: "hi"}}
	modified, got := em.Handle(nil, history)
	if modified {
		t.Fatal("nil error should never be treated as modified")
	}
	if len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("history should be returned unchanged, got %+v", got)
	}
}

func TestErrorManagerUnrelatedErrorIsNoOp(t *testing.T) {
	em := NewErrorManager()
	history := []core.Message{{Role: core.RoleUser, Content: "hi"}}
	modified, got := em.Handle(errors.New("connection refused"), history)
	if modified {
		t.Fatal("an unrelated error should not trigger the history rewrite")
	}
	if len(got) != 1 {
		t.Fatalf("history should be returned unchanged, got %+v", got)
	}
}

func TestErrorManagerStripsToolCallsOnThoughtSignatureError(t *testing.T) {
	em := NewErrorManager()
	history := []core.Message{
		{Role: core.RoleUser, Content: "add a function"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "c1", Function: core.FunctionCall{Name: "write_file", Arguments: `{"path":"x.go"}`}},
		}},
		{Role: core.RoleTool, ToolCallID: "c1", Content: "wrote 10 bytes"},
	}
	modified, got := em.Handle(errors.New(`Function call is missing a thought_signature in functionCall parts`), history)
	if !modified {
		t.Fatal("expected the thought_signature error to trigger a history rewrite")
	}
	if len(got) != 3 {
		t.Fatalf("expected the same number of messages, got %d", len(got))
	}
	if len(got[1].ToolCalls) != 0 {
		t.Errorf("expected ToolCalls stripped from the assistant message, got %+v", got[1].ToolCalls)
	}
	if got[1].ContentString() == "" {
		t.Error("expected the stripped tool call to be preserved as plain text content")
	}
	if got[2].Role != core.RoleUser {
		t.Errorf("expected the tool response converted to a user message, got role %q", got[2].Role)
	}
	if got[2].ToolCallID != "" {
		t.Error("expected ToolCallID cleared on the converted message")
	}
}

func TestErrorManagerAlsoTriggersOnInvalidArgument(t *testing.T) {
	em := NewErrorManager()
	history := []core.Message{
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "c1", Function: core.FunctionCall{Name: "read_file", Arguments: `{}`}},
		}},
	}
	modified, _ := em.Handle(errors.New(`API error 400: {"error":{"status":"INVALID_ARGUMENT"}}`), history)
	if !modified {
		t.Fatal("expected an INVALID_ARGUMENT error to trigger the same history rewrite as thought_signature")
	}
}

func TestErrorManagerOriginalHistoryIsNotMutated(t *testing.T) {
	em := NewErrorManager()
	original := []core.Message{
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c1", Function: core.FunctionCall{Name: "x"}}}},
	}
	_, _ = em.Handle(errors.New("INVALID_ARGUMENT"), original)
	if len(original[0].ToolCalls) != 1 {
		t.Error("Handle must not mutate the caller's original history slice/messages")
	}
}
