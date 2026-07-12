package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
)

func newTestSystem(t *testing.T) *System {
	t.Helper()
	sys, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	t.Cleanup(sys.Shutdown)
	return sys
}

func addEpisodic(t *testing.T, sys *System, goal, outcome, output string, toolsUsed []string, age time.Duration) {
	t.Helper()
	if err := sys.EpisodicAdd(core.EpisodicEntry{
		TaskGoal:  goal,
		Outcome:   outcome,
		Summary:   goal,
		Output:    output,
		ToolsUsed: toolsUsed,
		Timestamp: time.Now().Add(-age),
	}); err != nil {
		t.Fatalf("EpisodicAdd(%q): %v", goal, err)
	}
}

func TestRecallRanksByRelevance(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "implement user authentication with JWT", "success", "done", nil, time.Hour)
	addEpisodic(t, sys, "deploy the app to production", "success", "done", nil, time.Hour)

	r := NewHybridRetriever(sys, nil)
	hits := r.Recall("add JWT authentication", 5)
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if !strings.Contains(hits[0].Goal, "authentication") {
		t.Errorf("top hit = %q, want the authentication entry ranked first", hits[0].Goal)
	}
}

func TestRecallEmptyQueryReturnsNil(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "something", "success", "done", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	if hits := r.Recall("", 5); hits != nil {
		t.Errorf("Recall(\"\") = %v, want nil", hits)
	}
	if hits := r.Recall("something", 0); hits != nil {
		t.Errorf("Recall(query, 0) = %v, want nil", hits)
	}
}

func TestRecallClampsK(t *testing.T) {
	sys := newTestSystem(t)
	for i := 0; i < 30; i++ {
		addEpisodic(t, sys, "build a calculator app", "success", "done", nil, time.Duration(i)*time.Minute)
	}
	r := NewHybridRetriever(sys, nil)
	hits := r.Recall("build a calculator app", 1000)
	if len(hits) > 20 {
		t.Errorf("Recall with k=1000 returned %d hits, want <= 20 (clamp)", len(hits))
	}
}

func TestExactRecallNormalizedMatch(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "Fix the bug.", "success", "the answer", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	// Trivial rephrasing (case, trailing punctuation, whitespace) must still hit.
	out, ok := r.ExactRecall("  fix the bug  ", 0)
	if !ok || out != "the answer" {
		t.Errorf("ExactRecall normalized match: got (%q, %v), want (\"the answer\", true)", out, ok)
	}
}

func TestExactRecallExcludesToolUsingTasks(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "run the tests", "success", "all green", []string{"terminal"}, time.Hour)
	r := NewHybridRetriever(sys, nil)

	if _, ok := r.ExactRecall("run the tests", 0); ok {
		t.Error("ExactRecall must never cache tool-using tasks")
	}
}

func TestExactRecallExcludesFailuresAndEmptyOutput(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "explain recursion", "failure", "", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	if _, ok := r.ExactRecall("explain recursion", 0); ok {
		t.Error("ExactRecall must not match a failed task")
	}
}

func TestExactRecallRespectsMaxAge(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "what is 2+2", "success", "4", nil, 48*time.Hour)
	r := NewHybridRetriever(sys, nil)

	if _, ok := r.ExactRecall("what is 2+2", 24*time.Hour); ok {
		t.Error("ExactRecall must respect maxAge and reject a stale entry")
	}
	if _, ok := r.ExactRecall("what is 2+2", 0); !ok {
		t.Error("ExactRecall with maxAge=0 (no limit) should still match")
	}
}

func TestConfidentRecallNearDuplicateMatch(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "fix the authentication login bug", "success", "fixed it", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	// Reworded/reordered near-duplicate (same 4 significant tokens: fix,
	// authentication, login, bug) should still hit via the Jaccard fallback.
	out, ok := r.ConfidentRecall("fix the login authentication bug", 0)
	if !ok || out != "fixed it" {
		t.Errorf("ConfidentRecall near-duplicate: got (%q, %v), want (\"fixed it\", true)", out, ok)
	}
}

func TestConfidentRecallRejectsTopicalOnlyMatch(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "fix the authentication login bug", "success", "fixed it", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	// Shares "login" but is a substantively different request — must NOT
	// skip the LLM for this (see AI_OPTIMIZATION_REPORT.md §4.1 for why this
	// threshold is deliberately strict).
	if _, ok := r.ConfidentRecall("redesign the login page layout", 0); ok {
		t.Error("ConfidentRecall must not match a topically-related-but-different request")
	}
}

func TestConfidentRecallExactShortQueryStillMatchesViaExactPath(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "fix it", "success", "fixed", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	// An identical short query still matches — via ExactRecall (literal
	// identity is always safe, regardless of length), which ConfidentRecall
	// tries before falling back to the token-count-gated fuzzy path.
	out, ok := r.ConfidentRecall("fix it", 0)
	if !ok || out != "fixed" {
		t.Errorf("ConfidentRecall(identical short query) = (%q, %v), want (\"fixed\", true) via ExactRecall", out, ok)
	}
}

func TestConfidentRecallShortNonIdenticalQueryNeverFuzzyMatches(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "fix the login", "success", "fixed", nil, time.Hour)
	r := NewHybridRetriever(sys, nil)

	// Not an exact match, and below confidentRecallMinTokens — too short to
	// reliably distinguish "same request" from coincidence via Jaccard, so
	// it must NOT fuzzy-match even though every token it has overlaps.
	if _, ok := r.ConfidentRecall("fix login now", 0); ok {
		t.Error("ConfidentRecall should not fuzzy-match a short, non-identical query")
	}
}

func TestFormatRecallCapsSize(t *testing.T) {
	var hits []RecallHit
	for i := 0; i < 50; i++ {
		hits = append(hits, RecallHit{
			Source:  "episodic",
			Goal:    strings.Repeat("goal text ", 10),
			Snippet: strings.Repeat("snippet text ", 20),
		})
	}
	out := FormatRecall(hits)
	if len(out) > maxRecallBlockLen+200 { // small slack for the final summary line
		t.Errorf("FormatRecall output length = %d, want roughly <= %d", len(out), maxRecallBlockLen)
	}
	if !strings.Contains(out, "more result(s) omitted") {
		t.Error("expected FormatRecall to summarize omitted hits rather than silently dropping them")
	}
}

func TestFormatRecallEmpty(t *testing.T) {
	if out := FormatRecall(nil); out != "" {
		t.Errorf("FormatRecall(nil) = %q, want \"\"", out)
	}
}

func TestKGBoostedRecallRanksHigher(t *testing.T) {
	sys := newTestSystem(t)
	kg := newTestKG(t)

	// Two entries with similar raw token overlap to the query, but only one
	// shares a KG concept relation with it.
	addEpisodic(t, sys, "improve database query performance", "success", "done", nil, 2*time.Hour)
	addEpisodic(t, sys, "improve error message clarity", "success", "done", nil, time.Hour)

	if err := kg.RecordWordRelations("database performance tuning requires indexing"); err != nil {
		t.Fatalf("RecordWordRelations: %v", err)
	}

	r := NewHybridRetriever(sys, kg)
	hits := r.Recall("database indexing performance", 5)
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}
	if !strings.Contains(hits[0].Goal, "database") {
		t.Errorf("top hit = %q, want the KG-corroborated database entry ranked first", hits[0].Goal)
	}
}
