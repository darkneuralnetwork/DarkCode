package memory

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/darkcode/core"
)

// countingEmbedder counts how many times a vector was actually computed.
type countingEmbedder struct{ calls int32 }

func (c *countingEmbedder) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	atomic.AddInt32(&c.calls, 1)
	return []float32{float32(len(text)), 1, 2}, nil
}
func (c *countingEmbedder) ChatCompletion(context.Context, *core.CompletionRequest) (*core.CompletionResponse, error) {
	return nil, nil
}
func (c *countingEmbedder) ChatCompletionStream(context.Context, *core.CompletionRequest, *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return nil, nil
}
func (c *countingEmbedder) ModelInfo() core.ModelMetadata { return core.ModelMetadata{} }
func (c *countingEmbedder) Ping(context.Context) error    { return nil }
func (c *countingEmbedder) Close() error                  { return nil }
func (c *countingEmbedder) n() int                        { return int(atomic.LoadInt32(&c.calls)) }

func embedSystem(t *testing.T) (*System, *countingEmbedder) {
	t.Helper()
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	e := &countingEmbedder{}
	s.SetEmbedder(e)
	return s, e
}

// TestTheSameQueryIsEmbeddedOnce is the regression, and it is a per-REQUEST
// cost. There was no cache at all: the cognition cascade embeds the goal for
// ConfidentRecall, the recall block embeds the identical goal again for
// HybridRetriever, and the plan gate embeds it a third time — three network
// round-trips for one string.
func TestTheSameQueryIsEmbeddedOnce(t *testing.T) {
	s, e := embedSystem(t)
	const goal = "why does the retry layer back off on 429"

	for i := 0; i < 5; i++ {
		if _, err := s.GetEmbedding(goal); err != nil {
			t.Fatal(err)
		}
	}
	if e.n() != 1 {
		t.Errorf("the same text was embedded %d times, want 1", e.n())
	}
}

// TestDifferentTextStillEmbeds — a cache that returns a vector for different
// words is a wrong answer, not a fast one.
func TestDifferentTextStillEmbeds(t *testing.T) {
	s, e := embedSystem(t)
	for _, q := range []string{"alpha", "beta", "gamma", "alpha"} {
		if _, err := s.GetEmbedding(q); err != nil {
			t.Fatal(err)
		}
	}
	if e.n() != 3 {
		t.Errorf("embedded %d distinct texts, want 3", e.n())
	}
}

// TestCachedVectorMatchesComputed — the memo must return the same vector the
// embedder produced, or retrieval silently ranks against the wrong numbers.
func TestCachedVectorMatchesComputed(t *testing.T) {
	s, _ := embedSystem(t)
	first, err := s.GetEmbedding("some query")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := s.GetEmbedding("some query")
	if len(first) != len(second) {
		t.Fatalf("cached vector has a different length: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("cached vector differs at %d: %v vs %v", i, first[i], second[i])
		}
	}
}

// TestCacheIsBounded — an agent that runs for hours must not accumulate a
// vector per distinct string it ever saw.
func TestCacheIsBounded(t *testing.T) {
	s, _ := embedSystem(t)
	for i := 0; i < embedCacheMax*3; i++ {
		if _, err := s.GetEmbedding(fmt.Sprintf("query-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.RLock()
	n := len(s.embedCache)
	s.mu.RUnlock()
	if n > embedCacheMax {
		t.Errorf("cache holds %d entries, past the %d bound", n, embedCacheMax)
	}
}

// TestConcurrentEmbeddingIsSafe — retrieval runs on the request path and
// sub-agents run in parallel.
func TestConcurrentEmbeddingIsSafe(t *testing.T) {
	s, _ := embedSystem(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.GetEmbedding(fmt.Sprintf("q-%d", i%5))
		}(i)
	}
	wg.Wait()
}

// TestFailedEmbeddingIsNotCached — caching an error as a vector would make one
// transient failure permanent.
func TestFailedEmbeddingIsNotCached(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	if _, err := s.GetEmbedding("x"); err == nil {
		t.Fatal("expected an error with no embedder configured")
	}
	s.mu.RLock()
	n := len(s.embedCache)
	s.mu.RUnlock()
	if n != 0 {
		t.Errorf("a failed embedding was cached (%d entries)", n)
	}
}
