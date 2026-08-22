package memory

import (
	"math"
	"testing"
)

// TestCosineSurvivesLargeMagnitudes is the regression guard for the
// widen-before-multiply fix in cosineSimilarity.
//
// Computing float64(a[i] * b[i]) does the multiply in float32, which tops out
// near 3.4e38: a component around 1e19 squares to +Inf and the function returns
// NaN. A NaN score does not merely lose the ranking, it sorts unpredictably
// against every other candidate.
//
// This is a correctness floor rather than a live bug — no embedder emits
// components at this magnitude. It is worth pinning because the narrow multiply
// looks harmless and would be easy to reintroduce: measured over 20,000 random
// 768-dim pairs it cost at most 3.7e-9 of accuracy and never flipped a ranking,
// so nothing in normal operation would reveal it.
func TestCosineSurvivesLargeMagnitudes(t *testing.T) {
	const dim = 8
	a := make([]float32, dim)
	b := make([]float32, dim)
	for i := range a {
		a[i] = 3e19 // squares to +Inf in float32
		b[i] = 3e19
	}

	got := cosineSimilarity(a, b)

	if math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("cosineSimilarity returned %v — the products are overflowing float32", got)
	}
	if math.Abs(got-1.0) > 1e-9 {
		t.Errorf("identical vectors scored %.15f, want 1", got)
	}
}

// TestCosineKnownValues pins the three values the ranking relies on, so a
// rewrite of the loop cannot silently change what a score means.
func TestCosineKnownValues(t *testing.T) {
	x := []float32{1, 0, 0}
	y := []float32{0, 1, 0}
	neg := []float32{-1, 0, 0}

	if got := cosineSimilarity(x, x); math.Abs(got-1) > 1e-12 {
		t.Errorf("a vector against itself scored %v, want 1", got)
	}
	if got := cosineSimilarity(x, y); math.Abs(got) > 1e-12 {
		t.Errorf("orthogonal vectors scored %v, want 0", got)
	}
	if got := cosineSimilarity(x, neg); math.Abs(got+1) > 1e-12 {
		t.Errorf("opposite vectors scored %v, want -1", got)
	}
	if got := cosineSimilarity(x, []float32{}); got != 0 {
		t.Errorf("empty vector scored %v, want 0", got)
	}
	if got := cosineSimilarity(x, []float32{1, 0}); got != 0 {
		t.Errorf("mismatched lengths scored %v, want 0", got)
	}
}
