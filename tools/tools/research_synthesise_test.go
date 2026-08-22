package tools

// research_synthesise_test.go — synthesise had no test coverage before
// migrating it onto modelport.CompleteWith. These lock in the request shape
// (ceiling, temperature — previously unpinned, now PurposeCompress's 0.1)
// and the soft-fail-to-empty-string contract on any failure.

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/modelport"
)

type synthClient struct {
	reply string
	err   error
	got   *core.CompletionRequest
}

func (c *synthClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	c.got = req
	if c.err != nil {
		return nil, c.err
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: c.reply}}}}, nil
}
func (c *synthClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return c.ChatCompletion(ctx, req)
}
func (c *synthClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (c *synthClient) ModelInfo() core.ModelMetadata  { return core.ModelMetadata{} }
func (c *synthClient) Ping(ctx context.Context) error { return nil }
func (c *synthClient) Close() error                   { return nil }

type synthRouter struct {
	client  core.LLMClient
	gotTier core.ModelTier
}

func (r *synthRouter) Route(tier core.ModelTier, complexity int, desc string) (core.LLMClient, string, error) {
	r.gotTier = tier
	return r.client, "synth-model", nil
}
func (r *synthRouter) Consensus(ctx context.Context, msgs []core.Message, goal string) (*core.ConsensusResult, error) {
	return nil, nil
}
func (r *synthRouter) GetMode() core.RoutingMode { return core.RouteSingle }
func (r *synthRouter) ModelCount() int           { return 1 }

func TestSynthesiseRequestMatchesPurposeCompressPolicy(t *testing.T) {
	c := &synthClient{reply: "the answer [S1]"}
	rt := &ResearchTool{Router: &synthRouter{client: c}}

	got := rt.synthesise(context.Background(), "what is X?", "S1: X is Y")
	if got != "the answer [S1]" {
		t.Fatalf("synthesise() = %q, want the model's reply", got)
	}
	if c.got == nil {
		t.Fatal("client never received a request")
	}
	_, wantMaxTok, wantTemp := modelport.PolicyFor(modelport.PurposeCompress)
	if c.got.MaxTokens == nil || *c.got.MaxTokens != wantMaxTok {
		t.Errorf("MaxTokens = %v, want %d", c.got.MaxTokens, wantMaxTok)
	}
	if c.got.Temperature == nil || *c.got.Temperature != wantTemp {
		t.Errorf("Temperature = %v, want %f (previously unpinned — this is the intentional fix)", c.got.Temperature, wantTemp)
	}
}

func TestSynthesiseFailsSoftToEmptyString(t *testing.T) {
	for name, c := range map[string]*synthClient{
		"error":       {err: context.DeadlineExceeded},
		"empty reply": {reply: ""},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &ResearchTool{Router: &synthRouter{client: c}}
			if got := rt.synthesise(context.Background(), "q", "digest"); got != "" {
				t.Errorf("synthesise() = %q, want empty string on failure (caller falls back to extracts)", got)
			}
		})
	}
}

func TestSynthesiseNoRouterIsEmptyString(t *testing.T) {
	rt := &ResearchTool{}
	if got := rt.synthesise(context.Background(), "q", "digest"); got != "" {
		t.Errorf("synthesise() = %q, want empty string with no router configured", got)
	}
}
