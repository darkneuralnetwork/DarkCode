package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// TestGetRecallBlock_EpochScoping verifies the fresh-chat isolation fix: after
// StartNewSession, an episodic conversation from a previous session must no
// longer be injected into recall, while durable semantic facts are still kept.
func TestGetRecallBlock_EpochScoping(t *testing.T) {
	deps := newTestKernel(t, nil)

	// A prior-session conversation (episodic) and a durable fact (semantic),
	// both timestamped before the upcoming new-session boundary.
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal:  "tell me about quantum widgets",
		Outcome:   "success",
		Summary:   "explained quantum widgets",
		Output:    "Quantum widgets are imaginary.",
		Timestamp: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Memory.SemanticAdd("quantum widgets", "Quantum widgets are a durable documented fact.", "reference", []string{"quantum", "widgets"}); err != nil {
		t.Fatal(err)
	}

	goal := "tell me about quantum widgets"

	// Before a new session both are recallable.
	if block := deps.Kernel.getRecallBlock(goal); !strings.Contains(block, "widgets") {
		t.Fatalf("expected recall to surface prior context before new session, got: %q", block)
	}

	// Start a fresh session — this advances the epoch.
	deps.Memory.StartNewSession()

	block := deps.Kernel.getRecallBlock(goal)
	// The prior-session episodic conversation must be suppressed…
	if strings.Contains(block, "explained quantum widgets") || strings.Contains(block, "imaginary") {
		t.Fatalf("prior-session episodic should be suppressed after New Chat, got: %q", block)
	}
	// …but the durable semantic fact must still be available.
	if !strings.Contains(block, "durable documented fact") {
		t.Fatalf("durable semantic memory must survive a new session, got: %q", block)
	}
}

// TestGetRecallBlock_SuppressesPriorSessionTaskFacts is the follow-up fix: the
// kernel writes every Q&A into semantic memory keyed "task:<goal>". Those are
// prior conversations, not durable knowledge, so a fresh session must suppress
// the ones from before the epoch — the leak the user observed after /new.
func TestGetRecallBlock_SuppressesPriorSessionTaskFacts(t *testing.T) {
	deps := newTestKernel(t, nil)

	// Simulate what storeSemanticFacts writes for a prior chat: a "task:"-keyed
	// semantic entry with the answer in it.
	if err := deps.Memory.SemanticAdd(
		"task:who_is_prime_minister",
		"Goal: who is prime minister?\nOutcome: success\nResult: The Prime Minister of India is Narendra Modi.",
		"task", []string{"success", "task"},
	); err != nil {
		t.Fatal(err)
	}

	// New session — the prior "task:" fact predates the epoch.
	deps.Memory.StartNewSession()

	block := deps.Kernel.getRecallBlock("who is narendra modi?")
	if strings.Contains(block, "Narendra Modi") {
		t.Fatalf("prior-session task: semantic fact should be suppressed after New Chat, got: %q", block)
	}
}

// TestGetRecallBlock_KeepsCurrentSessionEpisodics confirms that an episodic
// entry created after the session boundary is still recalled (only *prior*
// sessions are suppressed).
func TestGetRecallBlock_KeepsCurrentSessionEpisodics(t *testing.T) {
	deps := newTestKernel(t, nil)
	deps.Memory.StartNewSession()

	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal:  "explain flux capacitors",
		Outcome:   "success",
		Summary:   "explained flux capacitors this session",
		Output:    "A flux capacitor makes time travel possible.",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if block := deps.Kernel.getRecallBlock("explain flux capacitors"); !strings.Contains(block, "flux capacitors") {
		t.Fatalf("current-session episodic should be recalled, got: %q", block)
	}
}
