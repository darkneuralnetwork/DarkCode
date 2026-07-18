package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// topicEmbedder returns orthogonal one-hot vectors per probe topic, so
// similar pairs score cosine 1 and dissimilar pairs 0 — a well-behaved
// embedder for the pass case.
type topicEmbedder struct{}

func (topicEmbedder) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	t := strings.ToLower(text)
	v := make([]float32, 8)
	switch {
	case strings.Contains(t, "login") || strings.Contains(t, "sign-in"):
		v[0] = 1
	case strings.Contains(t, "file") || strings.Contains(t, "storage"):
		v[1] = 1
	case strings.Contains(t, "deploy") || strings.Contains(t, "release"):
		v[2] = 1
	case strings.Contains(t, "cake"):
		v[3] = 1
	case strings.Contains(t, "weather"):
		v[4] = 1
	default:
		v[5] = 1
	}
	return v, nil
}
func (topicEmbedder) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return nil, context.Canceled
}
func (topicEmbedder) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return nil, context.Canceled
}
func (topicEmbedder) ModelInfo() core.ModelMetadata  { return core.ModelMetadata{ID: "topic"} }
func (topicEmbedder) Ping(ctx context.Context) error { return nil }
func (topicEmbedder) Close() error                   { return nil }

// degenerateEmbedder returns the same vector for every input — the failure
// mode this gate exists to catch (pooled chat-model embeddings with no
// sentence-level signal).
type degenerateEmbedder struct{ topicEmbedder }

func (degenerateEmbedder) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return []float32{1, 1, 1, 1}, nil
}

// failingEmbedder simulates a dead /embeddings endpoint.
type failingEmbedder struct{ topicEmbedder }

func (failingEmbedder) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, context.DeadlineExceeded
}

func TestValidateEmbedderPassesGoodEmbeddings(t *testing.T) {
	if err := ValidateEmbedder(context.Background(), topicEmbedder{}); err != nil {
		t.Fatalf("well-separated embeddings should validate: %v", err)
	}
}

func TestValidateEmbedderRejectsDegenerate(t *testing.T) {
	err := ValidateEmbedder(context.Background(), degenerateEmbedder{})
	if err == nil {
		t.Fatal("identical-vector embeddings must fail validation")
	}
	if !strings.Contains(err.Error(), "separate") {
		t.Fatalf("expected a margin diagnostic, got: %v", err)
	}
}

func TestValidateEmbedderRejectsFailingEndpoint(t *testing.T) {
	if err := ValidateEmbedder(context.Background(), failingEmbedder{}); err == nil {
		t.Fatal("a failing embeddings endpoint must fail validation")
	}
}
