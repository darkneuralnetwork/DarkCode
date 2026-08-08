package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/memory"
)

const corpusDir = "corpus/repo-memory-v1"

// floors are the scores this tree is known to reach. They are a ratchet in the
// same direction as .arch-baseline: a floor may rise, never fall.
//
// They are deliberately below the measured numbers rather than equal to them.
// A floor set to the exact current score turns every harmless ranking change
// into a red build, and a benchmark that cries wolf gets deleted. The purpose
// is to catch a real regression — a signal quietly stopping — not to pin
// three decimal places.
var floors = map[string]struct{ R5, MRR float64 }{
	"keyword":       {R5: 0.80, MRR: 0.65},
	"keyword+graph": {R5: 0.80, MRR: 0.72},
}

func load(t *testing.T) *Corpus {
	t.Helper()
	c, err := Load(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCorpusIsWellFormed runs first and separately, because every other result
// here is meaningless if a gold id names an entry that does not exist — the
// adapters would score zero and it would look like a retrieval failure.
func TestCorpusIsWellFormed(t *testing.T) {
	c := load(t)
	if len(c.Entries) < 10 || len(c.Queries) < 5 {
		t.Fatalf("corpus is too small to measure anything: %d entries, %d queries", len(c.Entries), len(c.Queries))
	}
	for _, q := range c.Queries {
		if q.Note == "" {
			t.Errorf("query %q has no note — a gold label nobody justified is a label nobody can argue with", q.Q)
		}
	}
}

// TestRetrievalScorecard is the benchmark. It prints the table and holds the
// floors.
//
// Both adapters run offline: no embedder is configured, so no model is called,
// nothing is billed, and the run is reproducible on a machine with no keys.
// They differ in exactly one variable — whether the knowledge graph is
// attached — so the gap between them is the graph's contribution.
func TestRetrievalScorecard(t *testing.T) {
	c := load(t)
	sys, err := Build(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sys.Shutdown)

	scores := []Score{
		Run("keyword", c, memory.NewHybridRetriever(sys, nil)),
		Run("keyword+graph", c, memory.NewHybridRetriever(sys, sys.KG())),
	}

	card := Scorecard(c, scores)
	t.Log("\n" + card)
	if out := os.Getenv("EVAL_SCORECARD"); out != "" {
		if err := os.WriteFile(filepath.Clean(out), []byte(card), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The claim under test. Attaching the graph must not make ordering worse,
	// and on this corpus it makes it better — the graph adapter reaches the
	// same R@5 with a higher R@1 and MRR, which is the precise shape of its
	// contribution: it does not find answers keyword missed, it promotes the
	// right one to the top. Asserting the relationship rather than a constant
	// keeps the check meaningful when the corpus grows.
	kw, kg := byAdapter(scores, "keyword"), byAdapter(scores, "keyword+graph")
	if kg.MRR < kw.MRR {
		t.Errorf("the graph made ordering worse: MRR %.3f with it, %.3f without", kg.MRR, kw.MRR)
	}
	if kg.R5 < kw.R5 {
		t.Errorf("the graph lost recall: R@5 %.3f with it, %.3f without", kg.R5, kw.R5)
	}
	if kg.Signals["keyword+kg"] == 0 {
		t.Error("no gold hit was attributed to the graph — the signal is not firing, and the " +
			"two adapters are measuring the same thing under different names")
	}

	for _, s := range scores {
		f, ok := floors[s.Adapter]
		if !ok {
			t.Errorf("adapter %q has no floor — an unfloored adapter can regress to zero quietly", s.Adapter)
			continue
		}
		if s.R5 < f.R5 {
			t.Errorf("%s R@5 = %.3f, below the floor of %.3f", s.Adapter, s.R5, f.R5)
		}
		if s.MRR < f.MRR {
			t.Errorf("%s MRR = %.3f, below the floor of %.3f", s.Adapter, s.MRR, f.MRR)
		}
	}
}

// TestScoringIsCorrect checks the metrics against a hand-computed case, because
// a scoring bug would make every number above confidently wrong — and a
// benchmark that is wrong in a flattering direction is worse than none.
func TestScoringIsCorrect(t *testing.T) {
	c := &Corpus{
		Name:    "hand",
		Entries: []Entry{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Queries: []Query{
			{Q: "first-place hit", Gold: []string{"a"}},
			{Q: "third-place hit", Gold: []string{"c"}},
			{Q: "missed entirely", Gold: []string{"b"}},
		},
	}
	// Ranked lists chosen so every metric has a different expected value.
	r := fakeRetriever{
		"first-place hit": {"a", "x", "y"},
		"third-place hit": {"x", "y", "c"},
		"missed entirely": {"x", "y", "z"},
	}

	s := Run("hand", c, r)
	// R@1: only the first query has gold at rank 1 → 1/3.
	assertClose(t, "R@1", s.R1, 1.0/3.0)
	// R@5: two of three queries have their single gold answer inside 5 → 2/3.
	assertClose(t, "R@5", s.R5, 2.0/3.0)
	// P@5: 1/5 for two queries, 0 for the third → (0.2+0.2+0)/3.
	assertClose(t, "P@5", s.P5, 0.4/3.0)
	// MRR: 1/1, 1/3, 0 → averaged.
	assertClose(t, "MRR", s.MRR, (1.0+1.0/3.0)/3.0)
	if len(s.Misses) != 1 || s.Misses[0] != "missed entirely" {
		t.Errorf("misses = %v, want the one query that found nothing", s.Misses)
	}
}

// TestGoldIdWithNoEntryIsRejected — the failure that would look like a
// retrieval bug forever.
func TestGoldIdWithNoEntryIsRejected(t *testing.T) {
	c := &Corpus{
		Name:    "broken",
		Entries: []Entry{{ID: "a"}},
		Queries: []Query{{Q: "q", Gold: []string{"nope"}}},
	}
	if err := c.validate(); err == nil {
		t.Fatal("a gold id naming no entry passed validation")
	}
}

type fakeRetriever map[string][]string

func (f fakeRetriever) Recall(q string, k int) []memory.RecallHit {
	var out []memory.RecallHit
	for i, id := range f[q] {
		if i >= k {
			break
		}
		out = append(out, memory.RecallHit{ID: id, Signal: "keyword"})
	}
	return out
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

func byAdapter(scores []Score, name string) Score {
	for _, s := range scores {
		if s.Adapter == name {
			return s
		}
	}
	return Score{Adapter: name}
}
