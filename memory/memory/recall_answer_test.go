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

// TestBestRecallAnswer_WorldFactsExpireWithoutTools corrects an assumption
// this file used to encode: that only TOOL-derived answers go stale, so a
// no-tool answer could be replayed forever. Whether a fact came from a web
// search or from the model's training data has nothing to do with whether it
// is still true — "who is prime minister" ages exactly the same either way.
// Staleness follows the SUBJECT, not the retrieval method (see replay.go).
func TestBestRecallAnswer_WorldFactsExpireWithoutTools(t *testing.T) {
	sys := newTestSystem(t)
	seedPMEpisode(t, sys, 30*24*time.Hour, nil)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.BestRecallAnswer("who is pm of india", 7*24*time.Hour); ok {
		t.Fatal("a month-old claim about who holds an office must not be replayed, tools or not")
	}
}

// TestBestRecallAnswer_SettledExplanationsDoNotExpire is the other half: a
// definitional answer that references neither the project nor the changeable
// world stays servable indefinitely. This is the saving the cache exists for.
func TestBestRecallAnswer_SettledExplanationsDoNotExpire(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "what is a mutex in concurrent programming", "success",
		"A mutex is a lock that lets one thread at a time enter a critical section.",
		nil, 90*24*time.Hour)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.BestRecallAnswer("what is a mutex in concurrent programming", 7*24*time.Hour); !ok {
		t.Fatal("a settled definitional answer must stay replayable regardless of age")
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
	// Phrased as a question, not "implement user authentication with JWT": a
	// command is never replayable (replay.go), so seeding one here would test
	// the admission gate rather than the vector path this case is about.
	addEpisodic(t, sys, "how does user authentication with JWT work", "success",
		"Auth flow explained: use JWT middleware.", nil, time.Hour)
	// Vectors land off the write path; this case is specifically about the
	// vector signal, so wait for it rather than racing the backfill.
	sys.WaitForEmbeddings()

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
