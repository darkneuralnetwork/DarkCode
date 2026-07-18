package orchestrator

// cascade_recall_test.go — rung 3 (confident episodic recall) acceptance:
// the motivating regression is "who is pm of india" erroring after the KG
// already recorded "The current Prime Minister of India is Narendra Modi" —
// rungs 1/2 can't match it (goal wording too different, not graph-shaped)
// and rung 3's injection-only recall needed a working LLM. Rung 3 now serves
// that stored answer directly, and the re-ask loop stays the escape hatch.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
)

const pmRecallAnswer = "The current Prime Minister of India is Narendra Modi, and the current Prime Minister of Japan is Sanae Takaichi (who took office in 2025)."

func seedPMRecall(t *testing.T, deps *testKernelDeps) {
	t.Helper()
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal:  "who is prime minister?",
		Outcome:   "success",
		Summary:   "answered a prime-minister lookup via web search",
		Output:    pmRecallAnswer,
		ToolsUsed: []string{"web_search"},
		Timestamp: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCascade_Rung3_RecallAnswersWithoutLLM(t *testing.T) {
	deps := newTestKernel(t, nil)
	seedPMRecall(t, deps)

	out, err := deps.Kernel.Execute(context.Background(), "who is pm of india")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deps.Client.callCount() != 0 {
		t.Fatalf("expected zero LLM calls, got %d", deps.Client.callCount())
	}
	if !strings.Contains(out, "Narendra Modi") {
		t.Fatalf("answer should carry the remembered fact, got: %s", out)
	}
	// The citation must disclose that this is a replayed memory.
	if !strings.Contains(out, "web_search") || !strings.Contains(out, "ask again") {
		t.Fatalf("answer must cite its origin and the re-ask escape hatch, got: %s", out)
	}

	log := deps.Kernel.CascadeLog()
	if len(log) != 1 {
		t.Fatalf("expected 1 cascade entry, got %d", len(log))
	}
	e := log[0]
	if !e.Answered || e.Rung != router.RungRecall || e.RungName != "recall" {
		t.Fatalf("wrong cascade entry: %+v", e)
	}
	if e.Confidence.Score < 0.75 {
		t.Fatalf("served answer must clear the default threshold, got %f", e.Confidence.Score)
	}
	if len(e.Confidence.Provenance) == 0 || !strings.HasPrefix(e.Confidence.Provenance[0], "episodic:") {
		t.Fatalf("recall answers must cite the episodic source, got %+v", e.Confidence.Provenance)
	}
}

func TestCascade_Rung3_ReAskEscalatesToLLM(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary", responses: []string{"fresh-llm-answer"}}
	deps := newTestKernel(t, client)
	seedPMRecall(t, deps)
	goal := "who is pm of india"

	if _, err := deps.Kernel.Execute(context.Background(), goal); err != nil {
		t.Fatal(err)
	}
	if client.callCount() != 0 {
		t.Fatalf("first ask should be served from recall, got %d LLM calls", client.callCount())
	}

	// Immediate re-ask: the replayed memory didn't satisfy — must escalate
	// to the LLM instead of serving the same recall answer again.
	if _, err := deps.Kernel.Execute(context.Background(), goal); err != nil {
		t.Fatal(err)
	}
	if client.callCount() == 0 {
		t.Fatal("re-ask must escalate to the LLM, not re-serve the rejected recall answer")
	}

	log := deps.Kernel.CascadeLog()
	last := log[len(log)-1]
	if last.Rung != router.RungLLM || last.RetryOfRungName != "recall" {
		t.Fatalf("re-ask entry should record the rejected rung, got %+v", last)
	}
	if !log[0].Retried {
		t.Fatal("the original recall answer must carry the negative label")
	}
}

func TestCascade_Rung3_ActionRequestsSkipRecall(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary", responses: []string{"done"}}
	deps := newTestKernel(t, client)
	// An episode whose text fully covers the action request — if the recall
	// rung were consulted it would match, so a zero-recall outcome proves the
	// entry-rung classifier routed the action straight to the LLM path.
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal:  "fix the login bug please",
		Outcome:   "success",
		Summary:   "fix login bug please",
		Output:    "fix login bug please done",
		Timestamp: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := deps.Kernel.Execute(context.Background(), "fix the login bug please"); err != nil {
		t.Fatal(err)
	}
	if client.callCount() == 0 {
		t.Fatal("action requests must reach the LLM/tool path, never a replayed answer")
	}
	log := deps.Kernel.CascadeLog()
	if len(log) != 1 || log[0].Answered || log[0].EntryRung != router.RungLLM {
		t.Fatalf("action request should enter (and escalate at) the LLM rung, got %+v", log)
	}
}
