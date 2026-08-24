package loop

// finding5_error_diagnostics_test.go — QA audit Finding 5. A real Gemini 400
// on the loop's iteration model call gave nothing to debug beyond the
// iteration number and the provider's generic message, and the outgoing
// request is never logged anywhere. This does not close Finding 5 (the root
// cause is still open, pending a retest against Gemini once its quota
// resets) — it only proves the error a future occurrence produces now names
// enough to start from: the iteration, how many tool calls preceded it, and
// which tools were offered.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/tools/tools"
)

func TestLoopIterationErrorIncludesDiagnostics(t *testing.T) {
	wantErr := errors.New(`API error 400: {"error":{"code":400,"message":"Request contains an invalid argument."}}`)
	client := &fakeLLMClient{failFn: func(req *core.CompletionRequest) error { return wantErr }}

	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)
	_, err := l.Run(context.Background(), "add a function and a test", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"iteration 0", "prior tool calls: 0", "tools offered:", wantErr.Error()} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q, got:\n%s", want, msg)
		}
	}
}
