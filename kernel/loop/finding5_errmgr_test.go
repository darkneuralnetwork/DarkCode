package loop

// finding5_errmgr_test.go — this is what actually closes QA audit Finding 5.
// loop.go had zero integration with agents.ErrorManager, the fix for
// Gemini's thought_signature/INVALID_ARGUMENT 400 on replayed tool-call
// history — agents.SubAgent (the DAG/trivial-task path) has always had it.
// This proves the loop now recovers from that exact error shape by retrying
// with sanitized history, instead of failing the whole task outright.

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/agents"
	"github.com/darkcode/tools/tools"
)

// historyWithAToolCall simulates a conversation already containing a
// replayed tool call — exactly the shape that trips Gemini's
// thought_signature requirement on the NEXT completion call. Handle only
// rewrites history that actually has ToolCalls/tool-role messages to find,
// so this seeds that rather than needing the fake client to script one.
func historyWithAToolCall() []core.Message {
	return []core.Message{
		{Role: core.RoleUser, Content: "earlier turn"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "c1", Function: core.FunctionCall{Name: "read_file", Arguments: `{"path":"x.go"}`}},
		}},
		{Role: core.RoleTool, ToolCallID: "c1", Content: "file contents"},
	}
}

func TestLoopRecoversFromInvalidArgumentViaErrorManager(t *testing.T) {
	client := &fakeLLMClient{
		failFn: func(req *core.CompletionRequest) error {
			for _, m := range req.Messages {
				if len(m.ToolCalls) > 0 {
					// Sanitized history reaches here without ToolCalls — the
					// retry attempt succeeds.
					return nil
				}
			}
			return &fakeAPIErr{msg: `API error 400: {"error":{"message":"Function call is missing a thought_signature in functionCall parts","status":"INVALID_ARGUMENT"}}`}
		},
	}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	l.SetErrorHandler(agents.NewErrorManager())

	res, err := l.Run(context.Background(), "continue the task", historyWithAToolCall())
	if err != nil {
		t.Fatalf("Run: %v (the errMgr-driven retry should have recovered from the INVALID_ARGUMENT error)", err)
	}
	if res == nil || res.Output == "" {
		t.Fatalf("expected a real answer after recovery, got %+v", res)
	}
}

func TestLoopWithoutErrorHandlerStillFailsOnInvalidArgument(t *testing.T) {
	// Control: confirms the fix is actually doing something — with no error
	// handler wired, the same error shape is NOT recovered, matching the
	// original Finding 5 behavior.
	client := &fakeLLMClient{failFn: func(req *core.CompletionRequest) error {
		return &fakeAPIErr{msg: `API error 400: INVALID_ARGUMENT`}
	}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	l.errMgr = nil

	if _, err := l.Run(context.Background(), "continue the task", historyWithAToolCall()); err == nil {
		t.Fatal("expected the run to fail with no error handler wired")
	}
}

type fakeAPIErr struct{ msg string }

func (e *fakeAPIErr) Error() string { return e.msg }
