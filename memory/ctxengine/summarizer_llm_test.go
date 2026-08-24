package ctxengine

// summarizer_llm_test.go — IncrementalSummarizer.Summarize's LLM branch had
// zero test coverage before, and used to be dead code in the running system:
// ctxengine.NewEngine is always constructed with a nil client (see engine.go
// — kept nil deliberately at construction, to avoid two independently-set
// client pointers), and nothing then wired a real one into the summarizer
// afterwards. Engine.SetClient now also calls IncrementalSummarizer.SetClient
// (compress.go), so the production engine (constructed via NewEngine(nil),
// then always immediately SetClient'd — see cmd/darkcode/app_wireup.go and
// kernel/loop/loop.go) does reach this branch, through
// Assemble → AdaptiveCompressor.Compress → this Summarize. Migrated onto
// modelport.CompleteWith for consistency with the rest of the codebase.

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/modelport"
)

type summarizerFakeClient struct {
	reply string
	got   *core.CompletionRequest
}

func (c *summarizerFakeClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	c.got = req
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: c.reply}}}}, nil
}
func (c *summarizerFakeClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return c.ChatCompletion(ctx, req)
}
func (c *summarizerFakeClient) CreateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (c *summarizerFakeClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "fake-model"}
}
func (c *summarizerFakeClient) Ping(context.Context) error { return nil }
func (c *summarizerFakeClient) Close() error               { return nil }

func TestIncrementalSummarizerLLMBranchUsesPurposeCompressPolicy(t *testing.T) {
	c := &summarizerFakeClient{reply: "a compact briefing"}
	s := NewIncrementalSummarizer(c)

	msg := s.Summarize(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hello"}})
	if c.got == nil {
		t.Fatal("client never received a request")
	}
	if c.got.Model != "fake-model" {
		t.Errorf("Model = %q, want the client's own model ID", c.got.Model)
	}
	_, wantMaxTok, wantTemp := modelport.PolicyFor(modelport.PurposeCompress)
	if c.got.MaxTokens == nil || *c.got.MaxTokens != wantMaxTok {
		t.Errorf("MaxTokens = %v, want %d", c.got.MaxTokens, wantMaxTok)
	}
	if c.got.Temperature == nil || *c.got.Temperature != wantTemp {
		t.Errorf("Temperature = %v, want %f", c.got.Temperature, wantTemp)
	}
	want := "[Summarized Context]\na compact briefing"
	if contentStr(msg) != want {
		t.Errorf("Summarize() content = %q, want %q", contentStr(msg), want)
	}
}

func TestIncrementalSummarizerFallsBackToExtractiveWithNoClient(t *testing.T) {
	s := NewIncrementalSummarizer(nil)
	msg := s.Summarize(context.Background(), []core.Message{{Role: core.RoleUser, Content: "This is a sentence. This is another sentence about files."}})
	if contentStr(msg) == "" {
		t.Fatal("expected a non-empty extractive fallback summary")
	}
}
