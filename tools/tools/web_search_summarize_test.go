package tools

// web_search_summarize_test.go — WebSearch's LLM-summarization branch had no
// test coverage before migrating it onto modelport.CompleteWith. These lock
// in the request shape (2000-token ceiling preserved, not silently switched
// to PurposeCompress's default 1200), the caller-supplied model (the
// "Gemini blank-name bug" the surrounding comment warns about), the LoRA
// mount/unmount bracketing, and the fallback-to-truncated-raw-results
// contract on any failure.

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

type webSearchClient struct {
	reply string
	err   error
	got   *core.CompletionRequest
	// mountScales records every MountLoRA call in order — WebSearch mounts
	// (scale 1.0) then unmounts (scale 0.0, deferred) around the completion
	// call, and the defer runs before this struct is inspected, so a single
	// "was it mounted" boolean would be overwritten by the unmount.
	mountScales []float32
}

func (c *webSearchClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	c.got = req
	if c.err != nil {
		return nil, c.err
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: c.reply}}}}, nil
}
func (c *webSearchClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return c.ChatCompletion(ctx, req)
}
func (c *webSearchClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (c *webSearchClient) ModelInfo() core.ModelMetadata  { return core.ModelMetadata{} }
func (c *webSearchClient) Ping(ctx context.Context) error { return nil }
func (c *webSearchClient) Close() error                   { return nil }
func (c *webSearchClient) MountLoRA(name string, scale float32) error {
	c.mountScales = append(c.mountScales, scale)
	return nil
}

type webSearchRouter struct{ client core.LLMClient }

func (r *webSearchRouter) Route(tier core.ModelTier, complexity int, desc string) (core.LLMClient, string, error) {
	return r.client, "web-search-model", nil
}
func (r *webSearchRouter) Consensus(ctx context.Context, msgs []core.Message, goal string) (*core.ConsensusResult, error) {
	return nil, nil
}
func (r *webSearchRouter) GetMode() core.RoutingMode { return core.RouteSingle }
func (r *webSearchRouter) ModelCount() int           { return 1 }

// longRawResultRegistry wires a search_files stand-in returning a long,
// deterministic string (no network) — routing the query through "local
// file" intent (classifySearchIntent) reaches this instead of a real
// Wikipedia/GitHub HTTP call, so the summarization branch is exercised
// without any network dependency.
func longRawResultRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(&ToolEntry{
		Name:     "search_files",
		ReadOnly: true,
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{Name: "search_files", Success: true, Output: strings.Repeat("relevant fact. ", 40)}
		},
	})
	return reg
}

func TestWebSearchSummarizeRequestPreservesTheExistingCeiling(t *testing.T) {
	c := &webSearchClient{reply: "concise summary"}
	wt := &WebTool{Router: &webSearchRouter{client: c}, Registry: longRawResultRegistry()}

	res := wt.WebSearch(context.Background(), map[string]interface{}{"query": "find this local file"})
	if !res.Success {
		t.Fatalf("WebSearch failed: %s", res.Error)
	}
	if c.got == nil {
		t.Fatal("client never received a summarization request")
	}
	if c.got.Model != "web-search-model" {
		t.Errorf("Model = %q, want the router-supplied model (the blank-name bug this guards against)", c.got.Model)
	}
	if c.got.MaxTokens == nil || *c.got.MaxTokens != 2000 {
		t.Errorf("MaxTokens = %v, want 2000 (preserved, not switched to PurposeCompress's default)", c.got.MaxTokens)
	}
	if len(c.mountScales) != 2 || c.mountScales[0] != 1.0 || c.mountScales[1] != 0.0 {
		t.Errorf("mountScales = %v, want [1.0, 0.0] (mount before the call, unmount deferred after)", c.mountScales)
	}
	if !strings.Contains(res.Output, "concise summary") {
		t.Errorf("expected the summarized output, got %q", res.Output)
	}
}

func TestWebSearchFallsBackToRawResultsOnSummarizeFailure(t *testing.T) {
	c := &webSearchClient{err: context.DeadlineExceeded}
	wt := &WebTool{Router: &webSearchRouter{client: c}, Registry: longRawResultRegistry()}

	res := wt.WebSearch(context.Background(), map[string]interface{}{"query": "find this local file"})
	if !res.Success {
		t.Fatalf("WebSearch failed: %s", res.Error)
	}
	if c.got == nil {
		t.Fatal("summarization was never attempted — this test proves nothing about the failure path without it")
	}
	if strings.Contains(res.Output, "concise summary") {
		t.Error("expected raw/truncated results, not a summarized answer, on LLM failure")
	}
}
