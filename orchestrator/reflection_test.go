package orchestrator

// reflection_test.go — Phase D acceptance tests (local-first upgrade §7):
// deterministic reflection rules, the prior-failure lookback that detects a
// proven fix, and the end-to-end effect — a fix fact written by one task
// answers a later question from the graph (rung 2) with zero LLM calls.

import (
	"context"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
	"github.com/darkcode/router"
)

func TestReflect_FailureLessons(t *testing.T) {
	deps := newTestKernel(t, nil)
	r := deps.Kernel.reflect("summarize the changelog", "", []string{"file_read"}, false, "direct")
	if r.Kind != ReflectionNone {
		t.Fatalf("a failure must never be classified as a fix/decision fact, got %v", r.Kind)
	}
	if len(r.Lessons) == 0 {
		t.Fatal("expected at least one lesson on failure")
	}
}

func TestReflect_FixKeywordNoPriorFailure(t *testing.T) {
	deps := newTestKernel(t, nil)
	r := deps.Kernel.reflect("fix the broken login button", "done", []string{"file_write"}, true, "direct")
	if r.Kind != ReflectionFix {
		t.Fatalf("expected ReflectionFix from the 'fix' keyword, got %v", r.Kind)
	}
	if r.ProblemGoal != "" {
		t.Fatalf("no prior failure was seeded — ProblemGoal must stay empty, got %q", r.ProblemGoal)
	}
}

func TestReflect_DecisionKeyword(t *testing.T) {
	deps := newTestKernel(t, nil)
	r := deps.Kernel.reflect("design the caching strategy for the API", "done", nil, true, "dag")
	if r.Kind != ReflectionDecision {
		t.Fatalf("expected ReflectionDecision from the 'design' keyword, got %v", r.Kind)
	}
}

func TestReflect_GenericSuccessNoKind(t *testing.T) {
	deps := newTestKernel(t, nil)
	r := deps.Kernel.reflect("summarize the changelog for the release", "done", nil, true, "direct")
	if r.Kind != ReflectionNone {
		t.Fatalf("a plain summary task should not be classified as fix/decision, got %v", r.Kind)
	}
	if len(r.Lessons) == 0 {
		t.Fatal("expected a generic success lesson")
	}
}

func TestReflect_PriorFailureLookbackDetectsProvenFix(t *testing.T) {
	deps := newTestKernel(t, nil)
	failGoal := "resolve the nil pointer crash in the session middleware"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: failGoal, Outcome: "failure", Summary: failGoal,
		Timestamp: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// A later, worded-differently but similar-enough successful attempt.
	r := deps.Kernel.reflect("resolve the nil pointer crash in session middleware", "added a nil check", []string{"file_write"}, true, "direct")
	if r.Kind != ReflectionFix {
		t.Fatalf("expected ReflectionFix from the prior-failure lookback, got %v", r.Kind)
	}
	if r.ProblemGoal != failGoal {
		t.Fatalf("ProblemGoal = %q, want %q", r.ProblemGoal, failGoal)
	}
	// Evidence fields (Phase D hardening): near-identical wording (only
	// "the" differs, a stopword the tokenizer already strips) must score
	// high similarity; neither goal mentions a file path, so no overlap;
	// the match was ~1 hour old.
	if r.Similarity < 0.85 {
		t.Fatalf("Similarity = %v, want >= 0.85 for near-identical wording", r.Similarity)
	}
	if r.FileOverlap {
		t.Fatal("neither goal mentions a file path — FileOverlap must be false")
	}
	if r.MatchAge < 55*time.Minute || r.MatchAge > 65*time.Minute {
		t.Fatalf("MatchAge = %v, want ~1 hour", r.MatchAge)
	}
}

func TestReflect_PriorFailureEvidence_FileOverlap(t *testing.T) {
	deps := newTestKernel(t, nil)
	failGoal := "resolve the crash in server/auth.go"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: failGoal, Outcome: "failure", Summary: failGoal,
		Output: "panic in server/auth.go", Timestamp: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	r := deps.Kernel.reflect("resolve the crash in server auth.go", "fixed server/auth.go by adding a nil check", nil, true, "direct")
	if r.ProblemGoal == "" {
		t.Fatal("expected the prior failure to be matched")
	}
	if !r.FileOverlap {
		t.Fatal("both the problem and the fix mention server/auth.go — FileOverlap must be true")
	}
}

func TestReflect_PriorFailureEvidence_NoFileOverlap(t *testing.T) {
	deps := newTestKernel(t, nil)
	failGoal := "resolve the crash in server/auth.go"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: failGoal, Outcome: "failure", Summary: failGoal,
		Output: "panic in server/auth.go", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// The fix mentions a different file — no genuine corroboration.
	r := deps.Kernel.reflect("resolve the crash in server auth.go", "fixed server/session.go instead", nil, true, "direct")
	if r.FileOverlap {
		t.Fatal("the fix touched a different file than the problem — FileOverlap must be false")
	}
}

func TestFixFactConfidence_ScalesWithEvidence(t *testing.T) {
	// Keyword-only fix (no prior-failure lookback evidence at all) gets the
	// base confidence with no bonuses.
	if got := fixFactConfidence(Reflection{Kind: ReflectionFix}); got != fixFactConfidenceBase {
		t.Fatalf("keyword-only fix confidence = %v, want base %v", got, fixFactConfidenceBase)
	}

	// A weak match (right at the similarity threshold, no file overlap, old)
	// gets no bonuses either — same as the base.
	weak := fixFactConfidence(Reflection{
		Kind: ReflectionFix, ProblemGoal: "x",
		Similarity: 0.6, FileOverlap: false, MatchAge: 3 * 24 * time.Hour,
	})
	if weak != fixFactConfidenceBase {
		t.Fatalf("weak-evidence confidence = %v, want base %v", weak, fixFactConfidenceBase)
	}

	// A strong match (near-identical wording, same file, fast turnaround)
	// gets every bonus, clamped at the max.
	strong := fixFactConfidence(Reflection{
		Kind: ReflectionFix, ProblemGoal: "x",
		Similarity: 0.9, FileOverlap: true, MatchAge: 10 * time.Minute,
	})
	if strong != fixFactConfidenceMax {
		t.Fatalf("strong-evidence confidence = %v, want max %v", strong, fixFactConfidenceMax)
	}
	if !(strong > weak) {
		t.Fatalf("stronger evidence must score higher: strong=%v weak=%v", strong, weak)
	}
}

func TestReflect_PriorFailureTooOldIsIgnored(t *testing.T) {
	deps := newTestKernel(t, nil)
	failGoal := "resolve the nil pointer crash in the session middleware"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: failGoal, Outcome: "failure", Summary: failGoal,
		Timestamp: time.Now().Add(-30 * 24 * time.Hour), // older than reflectLookbackWindow
	}); err != nil {
		t.Fatal(err)
	}

	r := deps.Kernel.reflect("resolve the nil pointer crash in session middleware", "added a nil check", nil, true, "direct")
	if r.ProblemGoal != "" {
		t.Fatalf("a stale failure must not be treated as fixed, got ProblemGoal=%q", r.ProblemGoal)
	}
}

func TestReflect_DissimilarPriorFailureIsIgnored(t *testing.T) {
	deps := newTestKernel(t, nil)
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: "deploy the release to the staging cluster", Outcome: "failure",
		Summary: "deploy the release to the staging cluster", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	r := deps.Kernel.reflect("resolve the nil pointer crash in session middleware", "added a nil check", nil, true, "direct")
	if r.ProblemGoal != "" {
		t.Fatalf("an unrelated prior failure must not be matched, got ProblemGoal=%q", r.ProblemGoal)
	}
}

// TestPhaseD_FixFactAnswersFutureQuestionWithoutLLM is the plan's stated
// Phase D effect: "future identical/similar tasks answer from rungs 1–3 →
// fewer LLM calls over time." A failed task followed by a successful retry
// on a similar goal writes a typed fix fact (recordOutcome →
// populateKnowledgeGraph); a later "how did we fix X" question must then
// answer from the knowledge graph (cascade rung 2) with ZERO LLM calls.
func TestPhaseD_FixFactAnswersFutureQuestionWithoutLLM(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary"}
	deps := newTestKernel(t, client)

	problemGoal := "resolve the nil pointer crash in the auth middleware"
	fixGoal := "resolve the nil pointer crash in auth middleware"

	// The problem task: recorded as a failure via the real promotion path so
	// its KG task node exists (recordOutcome, not the bare error-path
	// storeEpisodic, which never touches the KG).
	deps.Kernel.recordOutcome(problemGoal, "panic: nil pointer dereference", nil, false, "direct", 0, "")

	// The fix: a later, similarly-worded successful task. reflect() detects
	// the prior failure via GoalSimilarity and populateKnowledgeGraph writes
	// the fix node + fixed_by edge.
	deps.Kernel.recordOutcome(fixGoal, "added a nil check before dereferencing the session pointer", nil, true, "direct", 0, "")

	stats := deps.Kernel.CascadeStats()
	_ = stats // sanity that CascadeStats doesn't panic with no cascade activity yet

	before := client.callCount()
	out, err := deps.Kernel.Execute(context.Background(), "how did we fix the nil pointer crash in the auth middleware?")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if client.callCount() != before {
		t.Fatalf("expected zero LLM calls (answer should come from the fix fact), got %d new call(s)", client.callCount()-before)
	}
	if !containsAll(out, "nil check") {
		t.Fatalf("answer missing the recorded resolution: %s", out)
	}

	log := deps.Kernel.CascadeLog()
	if len(log) != 1 || log[0].Rung != 2 || !log[0].Answered {
		t.Fatalf("expected a rung-2 (graph) cascade hit, got %+v", log)
	}
}

// TestPhaseD_DemotedFactStopsAnsweringAfterRejection is the write-back
// governance acceptance test (local-first upgrade Phase D hardening): a fix
// fact that answers a question, then gets REJECTED (the user immediately
// re-asks it), must be demoted — and a further ask of the same question must
// no longer be served from that fact, reaching the LLM instead. The goal
// text is a concrete multi-word action (classifyGoalIntent → intentAction) so
// every escalation reaches the LLM rather than the clarification gate,
// keeping the LLM call count the unambiguous signal.
func TestPhaseD_DemotedFactStopsAnsweringAfterRejection(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary"}
	deps := newTestKernel(t, client)

	problemGoal := "resolve the nil pointer bug in the auth middleware"
	fixGoal := "resolve the nil pointer bug in auth middleware"
	question := "how did we fix the nil pointer bug in the auth middleware?"

	deps.Kernel.recordOutcome(problemGoal, "panic: nil pointer dereference", nil, false, "direct", 0, "")
	deps.Kernel.recordOutcome(fixGoal, "added a nil check before dereferencing the session pointer", nil, true, "direct", 0, "")

	fixID := "fix:" + fixGoal // matches populateKnowledgeGraph's ID scheme (strutil.TruncateID, no-op under 60 chars)
	nodeBefore, ok := deps.Memory.KG().GetNode(fixID)
	if !ok {
		t.Fatalf("expected fix node %q to exist", fixID)
	}
	baseline := nodeBefore.Confidence

	// First ask: answered from the fix fact, zero LLM calls.
	out, err := deps.Kernel.Execute(context.Background(), question)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if client.callCount() != 0 {
		t.Fatalf("expected zero LLM calls on the first ask, got %d", client.callCount())
	}
	if !containsAll(out, "nil check") {
		t.Fatalf("first answer missing the recorded resolution: %s", out)
	}

	// Second ask (immediate re-ask == rejection): must escalate — the
	// re-ask detector force-escalates AND demotes the specific fact.
	if _, err := deps.Kernel.Execute(context.Background(), question); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	afterSecond := client.callCount()
	if afterSecond == 0 {
		t.Fatal("the re-ask must escalate to the LLM, not re-serve the rejected fact")
	}

	nodeAfter, ok := deps.Memory.KG().GetNode(fixID)
	if !ok {
		t.Fatal("fix node must still exist after demotion (soft demotion, not deletion)")
	}
	if !(nodeAfter.Confidence < baseline) {
		t.Fatalf("fact confidence must drop after rejection: before=%v after=%v", baseline, nodeAfter.Confidence)
	}

	// Third-ask proof, checked directly against the mechanism rather than
	// through a full Execute() call: the second ask's escalated LLM answer
	// got its own answer-cache entry (rung 1, via recordOutcome) for the
	// literal question text, and a THIRD identical Execute() call would
	// legitimately be served from THAT cache — a separate, correct feature,
	// not the thing under test. What Phase D hardening promises is that the
	// demoted fact itself durably stops clearing the graph rung's bar: query
	// the graph directly and confirm its confidence now falls below the
	// threshold that gated the original (first-ask) answer.
	ga, ok := memory.AnswerFromGraph(deps.Memory.KG(), question)
	if !ok {
		t.Fatal("expected the graph to still recognize the question (fact demoted, not deleted)")
	}
	if ga.Confidence.Score >= baseline {
		t.Fatalf("post-rejection graph confidence (%v) must be lower than the original (%v)", ga.Confidence.Score, baseline)
	}
	threshold := deps.Kernel.rungThreshold(router.RungGraph)
	if ga.Confidence.Score >= threshold {
		t.Fatalf("demoted fact's confidence (%v) must fall below the graph rung's answer threshold (%v) so it stops answering", ga.Confidence.Score, threshold)
	}
}
