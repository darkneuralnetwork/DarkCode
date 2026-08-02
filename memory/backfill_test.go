package memory

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/darkcode/core"
)

// countingEmbedder returns a deterministic vector and records how many calls
// it took to get there.
type countingEmbedder struct {
	core.LLMClient
	calls atomic.Int64
	fail  bool
}

func (c *countingEmbedder) CreateEmbedding(_ context.Context, text string) ([]float32, error) {
	c.calls.Add(1)
	if c.fail {
		return nil, fmt.Errorf("embedder unavailable")
	}
	v := make([]float32, 8)
	for i := range v {
		v[i] = float32(len(text)%7) + float32(i)
	}
	return v, nil
}

// TestSetEmbedderBackfillsExistingEntries is the regression guard for DC-22.
//
// SetEmbedder used to store the client and return. Vectors were attached only
// on new writes, and the embedder is installed from a goroutine that validates
// its output first — up to a minute after startup. Everything written before
// that finished therefore had no vector permanently, and if validation failed,
// nothing ever did. The store stayed mixed forever, which is what kept the
// ranking bug firing on every query instead of during a brief window.
func TestSetEmbedderBackfillsExistingEntries(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	// Entries written before any embedder exists — the normal startup case.
	for i := 0; i < 5; i++ {
		s.EpisodicAdd(core.EpisodicEntry{
			ID: fmt.Sprintf("e%d", i), TaskGoal: fmt.Sprintf("goal %d", i), Summary: "did a thing",
		})
	}
	_ = s.SemanticAdd("k1", "some knowledge", "note", nil)

	for _, e := range s.EpisodicGet() {
		if len(e.Vector) != 0 {
			t.Fatal("setup is wrong: entries should start unvectored")
		}
	}

	emb := &countingEmbedder{}
	s.SetEmbedder(emb)
	s.WaitForEmbeddings()

	missing := 0
	for _, e := range s.EpisodicGet() {
		if len(e.Vector) == 0 {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d episodic entries still have no vector after the embedder arrived — "+
			"the store stays permanently mixed and cross-signal ranking stays incoherent", missing)
	}
	for _, sem := range s.SemanticAll() {
		if len(sem.Vector) == 0 {
			t.Errorf("semantic entry %q still has no vector", sem.Key)
		}
	}
}

// TestBackfillIsIdempotent — a second SetEmbedder must not re-embed everything.
func TestBackfillIsIdempotent(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	for i := 0; i < 3; i++ {
		s.EpisodicAdd(core.EpisodicEntry{ID: fmt.Sprintf("e%d", i), TaskGoal: "g", Summary: "s"})
	}

	emb := &countingEmbedder{}
	s.SetEmbedder(emb)
	s.WaitForEmbeddings()
	first := emb.calls.Load()

	s.SetEmbedder(emb)
	s.WaitForEmbeddings()

	if got := emb.calls.Load(); got > first {
		t.Errorf("a second SetEmbedder made %d more embedding calls; already-vectored entries must be skipped", got-first)
	}
}

// TestBackfillStopsWhenTheEmbedderFails — a failing embedder must not be
// hammered once per entry for the whole store.
func TestBackfillStopsWhenTheEmbedderFails(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	for i := 0; i < 50; i++ {
		s.EpisodicAdd(core.EpisodicEntry{ID: fmt.Sprintf("e%d", i), TaskGoal: "g", Summary: "s"})
	}

	emb := &countingEmbedder{fail: true}
	s.SetEmbedder(emb)
	s.WaitForEmbeddings()

	if got := emb.calls.Load(); got > 3 {
		t.Errorf("made %d calls against a failing embedder; the pass must give up rather than retry per entry", got)
	}
}

// TestQueryEmbeddingIsCached is the guard for DC-23.
//
// Every Recall embedded its query over the network, on a path the cascade takes
// for essentially every request. The scan it was blamed on is ~12 ms at 50,000
// vectors; the round-trip is what costs.
func TestQueryEmbeddingIsCached(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	emb := &countingEmbedder{}
	s.SetEmbedder(emb)
	s.WaitForEmbeddings()
	before := emb.calls.Load()

	for i := 0; i < 5; i++ {
		if _, err := s.GetEmbedding("how does the router pick a model"); err != nil {
			t.Fatal(err)
		}
	}
	// Case and surrounding whitespace should not defeat it.
	if _, err := s.GetEmbedding("  How Does The Router Pick A Model  "); err != nil {
		t.Fatal(err)
	}

	if got := emb.calls.Load() - before; got != 1 {
		t.Errorf("six equivalent queries caused %d embedding calls, want 1", got)
	}
}

// TestFailedEmbeddingIsNotCached — caching a failure would make a transient
// outage permanent for that query.
func TestFailedEmbeddingIsNotCached(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	emb := &countingEmbedder{fail: true}
	s.SetEmbedder(emb)
	s.WaitForEmbeddings()

	_, _ = s.GetEmbedding("q")
	before := emb.calls.Load()
	emb.fail = false
	if v, err := s.GetEmbedding("q"); err != nil || len(v) == 0 {
		t.Fatalf("the retry after recovery returned err=%v len=%d", err, len(v))
	}
	if emb.calls.Load() == before {
		t.Error("the failure was cached; a transient outage would be permanent for this query")
	}
}

// TestEmbedCacheIsBounded keeps the cache from becoming a leak on a long
// session with many distinct questions.
func TestEmbedCacheIsBounded(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	s.SetEmbedder(&countingEmbedder{})
	s.WaitForEmbeddings()

	for i := 0; i < embedCacheMax*3; i++ {
		if _, err := s.GetEmbedding(fmt.Sprintf("distinct query %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	s.mu.RLock()
	n := len(s.embedCache)
	s.mu.RUnlock()
	if n > embedCacheMax {
		t.Errorf("cache holds %d entries, above the %d cap", n, embedCacheMax)
	}
}
