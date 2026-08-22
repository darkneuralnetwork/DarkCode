package ctxengine

// compress_llm_test.go — ported from kernel/compression's compress_llm_test.go
// when Compress/Summarize moved onto Engine (see compress.go's package
// comment for why). CompressBlock had a torn-read bug that was fixed there
// (captured client under lock, then read the shared field directly again at
// the call site); Compress and Summarize already had the correct pattern.
// CompressBlock itself was NOT ported (confirmed dead, no caller outside its
// own test), so that regression test doesn't carry over — these exercise the
// two methods that did.

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

type compressFakeClient struct {
	reply string
	err   error
	got   *core.CompletionRequest
}

func (c *compressFakeClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	c.got = req
	if c.err != nil {
		return nil, c.err
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: c.reply}}}}, nil
}
func (c *compressFakeClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return c.ChatCompletion(ctx, req)
}
func (c *compressFakeClient) CreateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (c *compressFakeClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "fake-model"}
}
func (c *compressFakeClient) Ping(context.Context) error { return nil }
func (c *compressFakeClient) Close() error               { return nil }

func TestEngineCompressUsesTheCapturedClientNotTheStaleField(t *testing.T) {
	c := &compressFakeClient{reply: "goal: g\nactive_tasks: task1"}
	e := NewEngine(nil)
	e.SetClient(c, "model")
	e.SetUseLocal(false)

	snap, err := e.Compress(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi"}}, "goal")
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if c.got == nil {
		t.Fatal("the client passed to SetClient never received the request")
	}
	if snap.Goal != "g" {
		t.Errorf("Goal = %q, want %q", snap.Goal, "g")
	}
}

func TestEngineCompressAfterSetClientUsesTheNewClient(t *testing.T) {
	old := &compressFakeClient{reply: "goal: old"}
	e := NewEngine(nil)
	e.SetClient(old, "model")
	e.SetUseLocal(false)

	fresh := &compressFakeClient{reply: "goal: fresh"}
	e.SetClient(fresh, "new-model")

	snap, err := e.Compress(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi"}}, "goal")
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if old.got != nil {
		t.Error("the old client received a request after SetClient swapped it out")
	}
	if fresh.got == nil {
		t.Fatal("the new client (set via SetClient) never received the request")
	}
	if snap.Goal != "fresh" {
		t.Errorf("Goal = %q, want the new client's reply", snap.Goal)
	}
}

func TestEngineCompressFallsBackToHeuristicOnError(t *testing.T) {
	c := &compressFakeClient{err: context.DeadlineExceeded}
	e := NewEngine(nil)
	e.SetClient(c, "model")
	e.SetUseLocal(false)

	snap, err := e.Compress(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi there"}}, "goal")
	if err != nil {
		t.Fatalf("Compress should never return an error, got: %v", err)
	}
	if snap == nil || snap.Goal != "goal" {
		t.Fatalf("expected a heuristic fallback snapshot, got %+v", snap)
	}
}

func TestEngineSummarizeUsesTheCapturedClientNotTheStaleField(t *testing.T) {
	c := &compressFakeClient{reply: "a concise briefing"}
	e := NewEngine(nil)
	e.SetClient(c, "model")
	e.SetUseLocal(false)

	out, err := e.Summarize(context.Background(), "some long project context", "myproject")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if c.got == nil {
		t.Fatal("the client never received the request")
	}
	if out != "a concise briefing" {
		t.Errorf("Summarize() = %q, want the model's reply", out)
	}
}

func TestEngineSummarizeAfterSetClientUsesTheNewClient(t *testing.T) {
	old := &compressFakeClient{reply: "old"}
	e := NewEngine(nil)
	e.SetClient(old, "model")
	e.SetUseLocal(false)

	fresh := &compressFakeClient{reply: "fresh briefing"}
	e.SetClient(fresh, "new-model")

	out, err := e.Summarize(context.Background(), "context text", "focus")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if old.got != nil {
		t.Error("the old client received a request after SetClient swapped it out")
	}
	if out != "fresh briefing" {
		t.Errorf("Summarize() = %q, want the new client's reply", out)
	}
}

func TestEngineSummarizeFallsBackToHeuristicOnError(t *testing.T) {
	c := &compressFakeClient{err: context.DeadlineExceeded}
	e := NewEngine(nil)
	e.SetClient(c, "model")
	e.SetUseLocal(false)

	longText := strings.Repeat("word ", 5000)
	out, err := e.Summarize(context.Background(), longText, "")
	if err != nil {
		t.Fatalf("Summarize should never return an error, got: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty heuristic fallback summary")
	}
}

func TestEngineCompressNoClientUsesHeuristic(t *testing.T) {
	e := NewEngine(nil)

	snap, err := e.Compress(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi"}}, "goal")
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if snap.Goal != "goal" {
		t.Errorf("Goal = %q, want %q from the heuristic fallback", snap.Goal, "goal")
	}
}
