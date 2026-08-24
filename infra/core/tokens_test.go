package core

import (
	"strings"
	"testing"
)

// The defect that motivated consolidating four estimators into one: the
// word-counting heuristic read about 29% low on source code, because code has
// few whitespace-separated "words" for its length. An under-estimate is the
// harmful direction — it packs more into a context window than the provider
// will accept.
func TestEstimateTokensDoesNotUnderCountSourceCode(t *testing.T) {
	code := strings.Repeat(
		"func (s *Server) handleFoo(w http.ResponseWriter, r *http.Request) {\n"+
			"\tif r.Method != http.MethodPost {\n\t\treturn\n\t}\n}\n", 8)

	got := EstimateTokens(code)
	// ASCII bills at 4 bytes per token, so the estimate should track length.
	if want := len(code) / 4; got < want*9/10 {
		t.Errorf("EstimateTokens(code) = %d, under the ~%d ASCII baseline", got, want)
	}

	// The old word-based estimate for this input; kept as a regression floor.
	const oldWordBasedEstimate = 166
	if got <= oldWordBasedEstimate {
		t.Errorf("EstimateTokens(code) = %d, still at or below the under-counting estimate %d",
			got, oldWordBasedEstimate)
	}
}

// CJK must not be billed per byte: those scripts tokenize at roughly 1.5
// characters per token, so a byte-based count charges about double.
func TestEstimateTokensIsRuneAwareForCJK(t *testing.T) {
	cjk := strings.Repeat("这是一个测试字符串用于验证令牌估算的准确性。", 20)
	got := EstimateTokens(cjk)
	if byteBased := len(cjk) / 4; got >= byteBased {
		t.Errorf("EstimateTokens(CJK) = %d, no better than the byte-based %d", got, byteBased)
	}
	if runes := len([]rune(cjk)); got > runes {
		t.Errorf("EstimateTokens(CJK) = %d, above one token per character (%d)", got, runes)
	}
}

func TestEstimateTokensTracksEnglishProse(t *testing.T) {
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20)
	got := EstimateTokens(prose)
	lo, hi := len(prose)/5, len(prose)/3
	if got < lo || got > hi {
		t.Errorf("EstimateTokens(prose) = %d, want between %d and %d", got, lo, hi)
	}
}

func TestEstimateTokensEdgeCases(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty string = %d, want 0", got)
	}
	// Anything present costs at least one token; returning 0 would let a caller
	// treat a non-empty message as free and overfill the window.
	for _, s := range []string{"a", " ", "中", "🚀"} {
		if got := EstimateTokens(s); got < 1 {
			t.Errorf("EstimateTokens(%q) = %d, want at least 1", s, got)
		}
	}
}

// Growing the input must never shrink the estimate, or budget arithmetic that
// adds a message could conclude it has more room than before.
func TestEstimateTokensIsMonotonic(t *testing.T) {
	base := "package main\n\nfunc main() {}\n"
	prev := 0
	for i := 1; i <= 25; i++ {
		got := EstimateTokens(strings.Repeat(base, i))
		if got < prev {
			t.Fatalf("estimate fell from %d to %d when the input grew", prev, got)
		}
		prev = got
	}
}
