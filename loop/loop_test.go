package loop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
)

// fakeLLMClient is a scripted core.LLMClient — see orchestrator's identical
// helper for the rationale; duplicated here rather than shared across
// packages since Go doesn't allow importing another package's _test.go
// files, and this is small enough not to be worth a separate testutil
// package for two consumers.
type fakeLLMClient struct {
	name      string
	responses []string
	delay     time.Duration
	calls     int32
}

func (f *fakeLLMClient) nextContent() string {
	idx := int(atomic.AddInt32(&f.calls, 1)) - 1
	if len(f.responses) == 0 {
		return "final answer"
	}
	if idx < len(f.responses) {
		return f.responses[idx]
	}
	return f.responses[len(f.responses)-1]
}

func (f *fakeLLMClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return f.ChatCompletionStream(ctx, req, nil)
}

func (f *fakeLLMClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	content := f.nextContent()
	if cb != nil && cb.OnContent != nil {
		cb.OnContent(content)
	}
	return &core.CompletionResponse{
		Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: content}}},
	}, nil
}

func (f *fakeLLMClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (f *fakeLLMClient) ModelInfo() core.ModelMetadata { return core.ModelMetadata{ID: f.name} }
func (f *fakeLLMClient) Ping(ctx context.Context) error { return nil }
func (f *fakeLLMClient) Close() error                   { return nil }

func newTestRouter(client core.LLMClient) *router.Router {
	r := router.NewRouter(core.RouteSingle, nil)
	for _, tier := range []core.ModelTier{core.ModelTierCoding, core.ModelTierReasoning, core.ModelTierFast} {
		r.RegisterModel(tier, client, "fake-model")
	}
	r.MarkPrimary("fake-model")
	return r
}

func TestReActLoopSingleTurnNoTools(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"the final answer, no corrections needed."}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	result, err := l.Run(context.Background(), "answer a simple question")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output == "" {
		t.Error("expected a non-empty final answer")
	}
}

func TestReActLoopRespectsCancellationBeforeStarting(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"should never be reached"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := l.Run(ctx, "do something")
	if err == nil {
		t.Fatal("expected Run to return an error for an already-cancelled context")
	}
}

// TestReActLoopCancellationMidRun verifies the loop notices a context
// cancellation between iterations promptly rather than running to
// completion or hanging.
func TestReActLoopCancellationMidRun(t *testing.T) {
	client := &fakeLLMClient{delay: 200 * time.Millisecond, responses: []string{
		"first response", "second response", "third response",
	}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 20)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := l.Run(ctx, "do something slow")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a context-deadline error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %s after cancellation, want it to return promptly", elapsed)
	}
}

func TestReActLoopMaxLoopsCeiling(t *testing.T) {
	// A tool schema-less registry means the model can only ever respond with
	// content, no tool calls — so with maxLoops=1 the very first no-tool-call
	// response should end the loop rather than needing more iterations.
	client := &fakeLLMClient{responses: []string{"done in one shot"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 1)

	result, err := l.Run(context.Background(), "quick task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output == "" {
		t.Error("expected a non-empty answer within the loop ceiling")
	}
}

func TestNewClampsInvalidMaxLoops(t *testing.T) {
	client := &fakeLLMClient{}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 0)
	if l.maxLoops != DefaultMaxLoops {
		t.Errorf("maxLoops = %d, want DefaultMaxLoops (%d) when constructed with 0", l.maxLoops, DefaultMaxLoops)
	}
}
