package memory

import (
	"fmt"
	"testing"
)

// TestRecallIsDeterministic guards the ranking against the store's map order.
//
// SemanticAll ranges over a map, so the candidate slice arrives in a different
// order on every call. fuse's per-signal sorts were stable, which PRESERVED
// that order for entries with equal scores — and RRF then assigned those
// entries different ranks, so the fused score itself differed between two
// identical queries against an unchanged store.
//
// The visible effect was that recall returned a different ranking each time it
// was asked the same question. That makes the answer cache non-reproducible,
// and it makes any before/after measurement of retrieval quality meaningless,
// because the list reshuffles on its own between the two measurements.
//
// Ranking must be a function of the store's contents alone.
func TestRecallIsDeterministic(t *testing.T) {
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	// Entries that tie on every signal: identical content, so identical keyword
	// overlap, no vectors, no graph hits. Ties are where the ordering was
	// decided by map iteration.
	for i := 0; i < 8; i++ {
		if err := s.SemanticAdd(fmt.Sprintf("parser-%d", i), "the parser builds a tree", "note", nil); err != nil {
			t.Fatal(err)
		}
	}

	r := NewHybridRetriever(s, s.KG())
	first := r.Recall("parser tree", 8)
	if len(first) == 0 {
		t.Fatal("no hits for a term that was just stored")
	}

	for call := 1; call <= 20; call++ {
		got := r.Recall("parser tree", 8)
		if len(got) != len(first) {
			t.Fatalf("call %d returned %d hits, the first returned %d", call, len(got), len(first))
		}
		for i := range got {
			if got[i].ID != first[i].ID {
				t.Fatalf("call %d ranked %q at position %d where the first call had %q — "+
					"the same question against an unchanged store returned a different ranking",
					call, got[i].ID, i, first[i].ID)
			}
			if got[i].Score != first[i].Score {
				t.Fatalf("call %d scored %q %v, the first call scored it %v — "+
					"the fused score depends on candidate arrival order, not on the store",
					call, got[i].ID, got[i].Score, first[i].Score)
			}
		}
	}
}
