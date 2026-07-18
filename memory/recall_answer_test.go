package memory

// recall_answer_test.go — rung-3 answerer eligibility and matching rules.
// The motivating scenario: "who is pm of india" must find the episode whose
// web_search answer says "The current Prime Minister of India is Narendra
// Modi …" even though goal-to-goal similarity is far below the rung-1 cache
// bar ("pm" tokenizes to nothing rung 1 can match).

import (
	"strings"
	"testing"
	"time"
)

const pmAnswer = "The current Prime Minister of India is Narendra Modi, and the current Prime Minister of Japan is Sanae Takaichi (who took office in 2025)."

func seedPMEpisode(t *testing.T, sys *System, age time.Duration, tools []string) {
	t.Helper()
	addEpisodic(t, sys, "who is prime minister?", "success", pmAnswer, tools, age)
}

func TestBestRecallAnswer_AcronymBridgesToAnswerText(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, time.Hour, []string{"web_search"})

	r := NewHybridRetriever(sys, nil)
	ra, ok := r.BestRecallAnswer("who is pm of india", 7*24*time.Hour)
	if !ok {
		t.Fatal("expected a recall answer: 'pm' should be bridged to 'prime minister' and 'india' is in the answer text")
	}
	if ra.Output != pmAnswer {
		t.Fatalf("wrong output: %q", ra.Output)
	}
	if ra.Score != recallAnswerKeywordScore {
		t.Fatalf("keyword match score = %v, want %v", ra.Score, recallAnswerKeywordScore)
	}
	if !strings.Contains(ra.Reason, "acronym") {
		t.Fatalf("reason should name the keyword/acronym signal, got %q", ra.Reason)
	}
}

func TestBestRecallAnswer_PartialCoverageMisses(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, time.Hour, []string{"web_search"})

	r := NewHybridRetriever(sys, nil)
	// "pakistan" appears nowhere in the stored answer — full coverage fails,
	// so this must escalate (topical overlap is context injection, not an
	// answer).
	if _, ok := r.BestRecallAnswer("who is pm of pakistan", 7*24*time.Hour); ok {
		t.Fatal("partial coverage must not answer")
	}
}

func TestBestRecallAnswer_MutatingToolsNeverAnswer(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, time.Hour, []string{"terminal"})

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.BestRecallAnswer("who is pm of india", 7*24*time.Hour); ok {
		t.Fatal("episodes that used non-read-only tools must never be replayed as answers")
	}
}

func TestBestRecallAnswer_ToolAnswersExpire(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, 8*24*time.Hour, []string{"web_search"})

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.BestRecallAnswer("who is pm of india", 7*24*time.Hour); ok {
		t.Fatal("a tool-derived answer older than toolMaxAge must not be served")
	}
}

func TestBestRecallAnswer_NoToolAnswersDoNotExpire(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, 30*24*time.Hour, nil)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.BestRecallAnswer("who is pm of india", 7*24*time.Hour); !ok {
		t.Fatal("toolMaxAge only bounds tool-derived answers, not pure no-tool ones")
	}
}

func TestBestRecallAnswer_RefusesThinQueriesAndArtifacts(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, time.Hour, nil)
	// An oversized output is a task artifact, not an answer.
	addEpisodic(t, sys, "long india report", "success",
		"india "+strings.Repeat("filler ", recallAnswerMaxOutputLen/7+1), nil, time.Hour)
	// Failures never answer.
	addEpisodic(t, sys, "who leads pakistan", "failure", "some text about pakistan leaders", nil, time.Hour)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.BestRecallAnswer("india?", 0); ok {
		t.Fatal("a one-content-token query must be refused")
	}
	if _, ok := r.BestRecallAnswer("long india report", 0); ok {
		t.Fatal("outputs over recallAnswerMaxOutputLen must be refused")
	}
	if _, ok := r.BestRecallAnswer("who leads pakistan", 0); ok {
		t.Fatal("failed episodes must be refused")
	}
}

func TestBestRecallAnswer_PrefersMostRecentOnTies(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "who is prime minister of india", "success", "Old answer: prime minister india placeholder.", nil, 48*time.Hour)
	seedPMEpisode(t, sys, time.Hour, nil) // newer, same keyword score

	r := NewHybridRetriever(sys, nil)
	ra, ok := r.BestRecallAnswer("who is pm of india", 0)
	if !ok {
		t.Fatal("expected an answer")
	}
	if ra.Output != pmAnswer {
		t.Fatalf("ties must keep the most recent answer, got %q", ra.Output)
	}
}

func TestBestRecallAnswer_VectorPathAnswersWithoutKeywordOverlap(t *testing.T) {
	sys := newTestSystem(t)
	sys.SetEmbedder(&fakeEmbedder{})
	// Written with the embedder active → the entry carries a vector.
	addEpisodic(t, sys, "implement user authentication with JWT", "success",
		"Auth flow explained: use JWT middleware.", nil, time.Hour)

	r := NewHybridRetriever(sys, nil)
	// Zero keyword coverage with the entry text ("sign-in", "security",
	// "check" appear nowhere) — only the embedding can connect these, and
	// fakeEmbedder maps both to the same topic vector (cosine 1.0).
	ra, ok := r.BestRecallAnswer("sign-in token security check", 0)
	if !ok {
		t.Fatal("expected the vector path to answer")
	}
	if ra.Score < 0.99 {
		t.Fatalf("cosine for same-topic fake vectors should be ~1.0, got %v", ra.Score)
	}
	if !strings.Contains(ra.Reason, "embedding") {
		t.Fatalf("reason should name the embedding signal, got %q", ra.Reason)
	}
}
