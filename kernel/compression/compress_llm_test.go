package compression

// compress_llm_test.go — Compress, CompressBlock and Summarize had zero
// direct unit test coverage before (only incidental coverage through
// orchestrator's integration tests). Added while auditing these call sites
// for the modelport migration: found and fixed a torn-read bug in
// CompressBlock and Summarize (both captured client := c.client under lock
// like Compress correctly does, but then read the shared field c.client
// directly again at the actual call site — an unsynchronized read racing
// SetClient's concurrent hot-swap). These tests exercise the normal
// (non-racing) call path end to end.

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
func (c *compressFakeClient) ModelInfo() core.ModelMetadata { return core.ModelMetadata{} }
func (c *compressFakeClient) Ping(context.Context) error    { return nil }
func (c *compressFakeClient) Close() error                  { return nil }

func TestCompressBlockUsesTheCapturedClientNotTheStaleField(t *testing.T) {
	c := &compressFakeClient{reply: "block summary"}
	comp := NewCompressor(c, "model", nil)
	comp.SetUseLocal(false)

	block, err := comp.CompressBlock(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi"}}, "goal")
	if err != nil {
		t.Fatalf("CompressBlock: %v", err)
	}
	if c.got == nil {
		t.Fatal("the client passed to NewCompressor never received the request")
	}
	if block.Summary != "block summary" {
		t.Errorf("Summary = %q, want the model's reply", block.Summary)
	}
}

func TestCompressBlockAfterSetClientUsesTheNewClient(t *testing.T) {
	old := &compressFakeClient{reply: "old"}
	comp := NewCompressor(old, "model", nil)
	comp.SetUseLocal(false)

	fresh := &compressFakeClient{reply: "fresh reply"}
	comp.SetClient(fresh, "new-model")

	block, err := comp.CompressBlock(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi"}}, "goal")
	if err != nil {
		t.Fatalf("CompressBlock: %v", err)
	}
	if old.got != nil {
		t.Error("the old client received a request after SetClient swapped it out")
	}
	if fresh.got == nil {
		t.Fatal("the new client (set via SetClient) never received the request")
	}
	if block.Summary != "fresh reply" {
		t.Errorf("Summary = %q, want the new client's reply", block.Summary)
	}
}

func TestSummarizeUsesTheCapturedClientNotTheStaleField(t *testing.T) {
	c := &compressFakeClient{reply: "a concise briefing"}
	comp := NewCompressor(c, "model", nil)
	comp.SetUseLocal(false)

	out, err := comp.Summarize(context.Background(), "some long project context", "myproject")
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

func TestSummarizeAfterSetClientUsesTheNewClient(t *testing.T) {
	old := &compressFakeClient{reply: "old"}
	comp := NewCompressor(old, "model", nil)
	comp.SetUseLocal(false)

	fresh := &compressFakeClient{reply: "fresh briefing"}
	comp.SetClient(fresh, "new-model")

	out, err := comp.Summarize(context.Background(), "context text", "focus")
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

func TestSummarizeFallsBackToHeuristicOnError(t *testing.T) {
	c := &compressFakeClient{err: context.DeadlineExceeded}
	comp := NewCompressor(c, "model", nil)
	comp.SetUseLocal(false)

	longText := strings.Repeat("word ", 5000)
	out, err := comp.Summarize(context.Background(), longText, "")
	if err != nil {
		t.Fatalf("Summarize should never return an error, got: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty heuristic fallback summary")
	}
}
