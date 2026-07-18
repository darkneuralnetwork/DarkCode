package memory

// retrieval_vector_test.go — Phase C acceptance tests (local-first upgrade):
// vector recall with a deterministic fake embedder, the legacy vectorless-
// entry regression (mixed-scorer thresholds), and graph-assisted retrieval
// (KG neighborhood expansion).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// fakeEmbedder is a deterministic core.LLMClient whose embeddings bucket text
// into three orthogonal topic vectors, so cosine similarity is predictable:
// auth-ish text → [1,0,0], deploy-ish → [0,1,0], everything else → [0,0,1].
type fakeEmbedder struct {
	fail bool // when true, CreateEmbedding errors (server-down simulation)
}

func (f *fakeEmbedder) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "auth") || strings.Contains(t, "jwt") ||
		strings.Contains(t, "sign-in") || strings.Contains(t, "token") ||
		strings.Contains(t, "login"):
		return []float32{1, 0, 0}, nil
	case strings.Contains(t, "deploy") || strings.Contains(t, "production"):
		return []float32{0, 1, 0}, nil
	default:
		return []float32{0, 0, 1}, nil
	}
}

func (f *fakeEmbedder) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return nil, context.Canceled
}
func (f *fakeEmbedder) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return nil, context.Canceled
}
func (f *fakeEmbedder) ModelInfo() core.ModelMetadata { return core.ModelMetadata{ID: "fake-embed"} }
func (f *fakeEmbedder) Ping(ctx context.Context) error { return nil }
func (f *fakeEmbedder) Close() error                   { return nil }

func TestRecallVectorSemanticMatch(t *testing.T) {
	sys := newTestSystem(t)
	sys.SetEmbedder(&fakeEmbedder{})

	// Written with the embedder active → entries carry vectors.
	addEpisodic(t, sys, "implement user authentication with JWT", "success", "done", nil, time.Hour)
	addEpisodic(t, sys, "deploy the app to production cluster", "success", "done", nil, time.Hour)

	r := NewHybridRetriever(sys, nil)
	// No keyword overlap with the auth entry ("sign-in", "token" don't appear
	// in its text) — only the vector path can connect these.
	hits := r.Recall("sign-in token security check", 5)
	if len(hits) == 0 {
		t.Fatal("expected a semantic hit with zero keyword overlap")
	}
	if !strings.Contains(hits[0].Goal, "authentication") {
		t.Errorf("top hit = %q, want the authentication entry via cosine similarity", hits[0].Goal)
	}
}

func TestRecallLegacyVectorlessEntriesSurviveEmbedderActivation(t *testing.T) {
	sys := newTestSystem(t)

	// Written BEFORE any embedder existed → no vector stored.
	addEpisodic(t, sys, "configure the nginx reverse proxy", "success", "done", nil, time.Hour)

	// Embedder comes online later (the exact situation after this upgrade
	// ships to an existing installation).
	sys.SetEmbedder(&fakeEmbedder{})

	r := NewHybridRetriever(sys, nil)
	hits := r.Recall("nginx proxy settings", 5)
	if len(hits) == 0 {
		t.Fatal("legacy vectorless entry was dropped once the embedder came online (mixed-threshold regression)")
	}
	if !strings.Contains(hits[0].Goal, "nginx") {
		t.Errorf("top hit = %q, want the nginx entry via keyword overlap", hits[0].Goal)
	}
}

func TestRecallEmbedderFailureFallsBackToKeywords(t *testing.T) {
	sys := newTestSystem(t)
	sys.SetEmbedder(&fakeEmbedder{fail: true}) // server down

	addEpisodic(t, sys, "configure the nginx reverse proxy", "success", "done", nil, time.Hour)

	r := NewHybridRetriever(sys, nil)
	hits := r.Recall("nginx proxy settings", 5)
	if len(hits) == 0 {
		t.Fatal("recall must degrade to keyword overlap when embedding fails")
	}
}

func TestRecallKGNeighborhoodBoost(t *testing.T) {
	sys := newTestSystem(t)

	// Graph: router ↔ advisor (concept co-occurrence edge).
	if err := sys.KG().RecordWordRelations("router advisor"); err != nil {
		t.Fatal(err)
	}

	// Two entries with IDENTICAL keyword overlap to the query ("router");
	// only one mentions the graph neighbor "advisor". The scheduler entry is
	// newer, so recency alone would rank it first — the neighborhood boost
	// must overcome that.
	addEpisodic(t, sys, "router config cleanup in advisor module", "success", "done", nil, 2*time.Hour)
	addEpisodic(t, sys, "router config cleanup in scheduler module", "success", "done", nil, time.Hour)

	r := NewHybridRetriever(sys, sys.KG())
	hits := r.Recall("router timeout bug", 5)
	if len(hits) < 2 {
		t.Fatalf("expected both entries, got %d", len(hits))
	}
	if !strings.Contains(hits[0].Goal, "advisor") {
		t.Errorf("top hit = %q, want the advisor entry boosted by KG neighborhood expansion", hits[0].Goal)
	}
}
