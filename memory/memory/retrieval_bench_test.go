package memory

import (
	"fmt"
	"testing"
)

// BenchmarkFuse measures the ranking step in isolation over a store-sized
// candidate set with a realistic mix of vectored, keyword-only and KG signals.
// It is the first benchmark in the repository: the retriever is the product's
// differentiator, so its cost is the one worth being able to watch for
// regression. fuse allocates three rank slices, a score map and an output
// slice, so its cost is O(n log n) in the candidate count — this pins that.
func BenchmarkFuse(b *testing.B) {
	for _, n := range []int{16, 128, 1024} {
		cands := make([]candidate, n)
		for i := range cands {
			c := cand(fmt.Sprintf("e-%04d", i), 0, 0, false)
			// Half carry a vector, a third also match on keyword, a tenth on KG,
			// so all three rank lists are exercised rather than one.
			if i%2 == 0 {
				c.vec, c.hasVec = 0.3+float64(i%50)/100.0, true
			}
			if i%3 == 0 {
				c.keyword = float64((i%7)+1) / 8.0
			}
			if i%10 == 0 {
				c.kgScore = float64((i % 5) + 1)
			}
			c.bonus = float64(i%15) / 100.0
			cands[i] = c
		}
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = fuse(cands, 10)
			}
		})
	}
}

// BenchmarkRecall measures end-to-end recall over a keyword-only store (no
// embedder wired, which is the cold-start case). It exists so a future change
// to the scan or the tokenizer has a number to move against.
func BenchmarkRecall(b *testing.B) {
	s, err := NewSystem(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Shutdown()
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("note-%04d", i)
		content := fmt.Sprintf("the parser builds a syntax tree for module %d and caches it", i)
		if err := s.SemanticAdd(key, content, "note", nil); err != nil {
			b.Fatal(err)
		}
	}
	r := NewHybridRetriever(s, s.KG())
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.Recall("parser syntax tree cache", 10)
	}
}
