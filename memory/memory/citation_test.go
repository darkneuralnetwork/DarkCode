package memory

import (
	"strings"
	"testing"
	"time"
)

func TestFormatRecallTagsAndSourcesEachFact(t *testing.T) {
	hits := []RecallHit{
		{Source: "episodic", ID: "ep_123", Goal: "add the parser", Snippet: "done"},
		{Source: "semantic", ID: "arch:layering", Goal: "layering rule", Snippet: "kernel below tools"},
	}
	block := FormatRecall(hits)

	for _, want := range []string{"[F1]", "[F2]", "(source: ep_123)", "(source: arch:layering)"} {
		if !strings.Contains(block, want) {
			t.Errorf("recall block missing %q:\n%s", want, block)
		}
	}
	// The instruction is what makes the tags mean anything.
	if !strings.Contains(block, "cite") {
		t.Error("recall block does not ask for citations")
	}
}

func TestCitedFacts(t *testing.T) {
	cases := map[string][]int{
		"Per [F1] the parser lives in parse.go": {1},
		"Both [F2] and [F1] agree":              {1, 2},
		"[F3] [F3] repeated":                    {3},
		"no citations here":                     nil,
		"[F0] and [Fx] are not valid":           nil,
	}
	for answer, want := range cases {
		got := CitedFacts(answer)
		if len(got) != len(want) {
			t.Errorf("CitedFacts(%q) = %v, want %v", answer, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("CitedFacts(%q) = %v, want %v", answer, got, want)
				break
			}
		}
	}
}

func TestUncitedClaimFlagsConfidentGuesses(t *testing.T) {
	// Structural claim, facts available, nothing cited → flag.
	if !UncitedClaim("The retry logic is defined in llm/retry.go.", 3) {
		t.Error("an uncited structural claim should be flagged")
	}
	// Same claim, but cited → fine.
	if UncitedClaim("Per [F2], the retry logic is defined in llm/retry.go.", 3) {
		t.Error("a cited claim must not be flagged")
	}
	// No facts were injected, so there was nothing to cite.
	if UncitedClaim("The retry logic is defined in llm/retry.go.", 0) {
		t.Error("nothing to cite means nothing to flag")
	}
	// General prose that asserts nothing structural.
	if UncitedClaim("That's a good idea; I'd start with the simplest version.", 3) {
		t.Error("an answer making no structural claim should not be flagged")
	}
}

// A recall block that would overflow the budget still has to be well-formed.
func TestFormatRecallTruncatesCleanly(t *testing.T) {
	var hits []RecallHit
	for i := 0; i < 50; i++ {
		hits = append(hits, RecallHit{
			Source: "semantic", ID: "k", Goal: strings.Repeat("x", 100),
			Snippet: strings.Repeat("y", 240), Timestamp: time.Now(),
		})
	}
	block := FormatRecall(hits)
	if len(block) > maxRecallBlockLen*2 {
		t.Errorf("recall block ran to %d bytes despite the budget", len(block))
	}
	if !strings.Contains(block, "omitted for length") {
		t.Error("truncation should say that results were dropped")
	}
}
