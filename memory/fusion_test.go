package memory

import (
	"strings"
	"testing"
	"time"
)

// oldRanking reproduces the scoring this replaced: each entry scored by EITHER
// cosine OR token overlap, both pushed into one list sorted by raw score.
// Kept in the test so the regression it fixes is demonstrable rather than
// asserted.
func oldRanking(cands []candidate) []string {
	type scored struct {
		id string
		s  float64
	}
	var out []scored
	for _, c := range cands {
		var s float64
		if c.hasVec {
			s = c.vec
			if s <= vectorFloor {
				continue
			}
		} else {
			s = c.keyword
			if s <= 0 {
				continue
			}
		}
		out = append(out, scored{c.hit.ID, s})
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].s > out[i].s {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	ids := make([]string, 0, len(out))
	for _, o := range out {
		ids = append(ids, o.id)
	}
	return ids
}

func cand(id string, vec, keyword float64, hasVec bool) candidate {
	return candidate{
		hit:     RecallHit{ID: id, Source: "semantic", Timestamp: time.Now()},
		vec:     vec,
		hasVec:  hasVec,
		keyword: keyword,
	}
}

// TestMixedScaleRankingIsFixed is the regression guard for DC-21.
//
// A mixed store — some entries vectored, some not — is the normal case, because
// nothing written before the embedder validated ever got a vector. In that
// store the two scorers produce different units and were compared directly.
//
// Here: a strong vector match (0.72 cosine, genuinely relevant) against a weak
// keyword match (0.80 of query tokens present, but only because the query is
// two common words). The old ranking puts the keyword hit first purely because
// 0.80 > 0.72 — a comparison with no meaning.
func TestMixedScaleRankingIsFixed(t *testing.T) {
	cands := []candidate{
		cand("strong-vector", 0.72, 0.05, true),
		cand("weak-keyword", 0, 0.80, false),
	}

	if got := oldRanking(cands); got[0] != "weak-keyword" {
		t.Fatalf("the old ranking should have put the keyword hit first (that is the bug); got %v", got)
	}

	fused := fuse(cands, 10)
	if len(fused) != 2 {
		t.Fatalf("expected both candidates, got %d", len(fused))
	}
	if fused[0].ID != "strong-vector" {
		t.Errorf("fused order = %s first, want strong-vector — a top-ranked vector hit must not lose to a weak keyword hit on raw score", fused[0].ID)
	}
}

// TestFusionRewardsAgreement — an entry that both signals rank highly should
// beat one that only a single signal likes. This is the property that makes
// fusion worth having at all.
func TestFusionRewardsAgreement(t *testing.T) {
	cands := []candidate{
		cand("both-agree", 0.65, 0.60, true),
		cand("vector-only", 0.70, 0.0, true),
		cand("keyword-only", 0, 0.90, false),
	}

	fused := fuse(cands, 10)
	if fused[0].ID != "both-agree" {
		t.Errorf("fused order = %v, want both-agree first: two signals agreeing is stronger evidence than either alone",
			ids(fused))
	}
	// The signal names every list the hit appeared in, so telemetry can say
	// which retrieval path earned the result rather than just "more than one".
	if !strings.Contains(fused[0].Signal, "vector") || !strings.Contains(fused[0].Signal, "keyword") {
		t.Errorf("Signal = %q, want it to name both contributing signals", fused[0].Signal)
	}
}

// TestSignalProvenanceIsRecorded — without this, "is RAG earning its cost?"
// cannot be answered, and the retrieval work cannot be falsified.
func TestSignalProvenanceIsRecorded(t *testing.T) {
	fused := fuse([]candidate{
		cand("v", 0.8, 0, true),
		cand("k", 0, 0.5, false),
	}, 10)

	got := map[string]string{}
	for _, h := range fused {
		got[h.ID] = h.Signal
	}
	if got["v"] != "vector" {
		t.Errorf("vector-only hit reported signal %q, want \"vector\"", got["v"])
	}
	if got["k"] != "keyword" {
		t.Errorf("keyword-only hit reported signal %q, want \"keyword\"", got["k"])
	}
}

// TestWeakVectorHitsAreStillExcluded — fusion must not resurrect noise. A
// cosine below the floor is not evidence, and rank alone would promote it.
func TestWeakVectorHitsAreStillExcluded(t *testing.T) {
	fused := fuse([]candidate{cand("noise", 0.05, 0, true)}, 10)
	if len(fused) != 0 {
		t.Errorf("a cosine of 0.05 was ranked; below the floor it is not evidence of anything (got %v)", ids(fused))
	}
}

// TestRecencyBreaksTiesWithoutDecidingThem — bonuses are scaled to nudge the
// order. If they dominated, a recent irrelevant entry would outrank an older
// exact match, which is how "helpful" recency ranking usually goes wrong.
func TestRecencyBreaksTiesWithoutDecidingThem(t *testing.T) {
	old := cand("old-strong", 0.9, 0.9, true)
	old.hit.Timestamp = time.Now().Add(-60 * 24 * time.Hour)
	recent := cand("recent-weak", 0.35, 0.05, true)
	recent.bonus = 0.15 // maximum recency bonus

	fused := fuse([]candidate{old, recent}, 10)
	if fused[0].ID != "old-strong" {
		t.Errorf("fused order = %v, want old-strong first: recency must break ties, not overturn relevance", ids(fused))
	}
}

// TestFusionRespectsK guards the caller's contract.
func TestFusionRespectsK(t *testing.T) {
	var cands []candidate
	for i := 0; i < 30; i++ {
		cands = append(cands, cand(strings.Repeat("x", i+1), 0.5, 0.5, true))
	}
	if got := len(fuse(cands, 5)); got != 5 {
		t.Errorf("fuse returned %d hits for k=5", got)
	}
	if got := len(fuse(nil, 5)); got != 0 {
		t.Errorf("fuse of nothing returned %d hits", got)
	}
}

func ids(hits []RecallHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}
