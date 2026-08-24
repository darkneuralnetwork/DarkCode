package ctxengine

// engine_test.go — Assemble had zero direct test coverage before Phase 3 of
// the context-management unification made it the canonical per-turn builder
// for pre-turn conversational history (loop.go, Chat). The chronological-
// restoration behavior tested here is itself part of that phase: Assemble
// used to hand back messages in relevance-rank order, not the order they
// were actually said in, which reads as a shuffled transcript to the model —
// see restoreChronological in engine.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

func TestAssembleRestoresChronologicalOrderWhenEverythingFits(t *testing.T) {
	e := NewEngine(nil)
	convo := []core.Message{
		{Role: core.RoleUser, Content: "let's talk about bananas"},
		{Role: core.RoleAssistant, Content: "sure, bananas are yellow"},
		{Role: core.RoleUser, Content: "totally unrelated: what is the capital of France"},
	}
	window, err := e.Assemble(context.Background(), AssembleRequest{
		Query:           "capital of France",
		Conversation:    convo,
		AvailableTokens: 10000,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// The query is most relevant to convo[2], so a pure relevance ranking
	// would put it first. Assemble must still emit convo's original order.
	if len(window.Messages) != len(convo) {
		t.Fatalf("got %d messages, want %d: %+v", len(window.Messages), len(convo), window.Messages)
	}
	for i, m := range window.Messages {
		if m.ContentString() != convo[i].ContentString() {
			t.Errorf("Messages[%d] = %q, want convo[%d] = %q (chronological order not preserved)",
				i, m.ContentString(), i, convo[i].ContentString())
		}
	}
}

func TestAssembleKeepsSurvivorsChronologicalAndSummaryLast(t *testing.T) {
	e := NewEngine(nil) // nil client: summarizer falls back to the extractive summary
	// 4 chars/token: each ~40-char message is ~10 tokens. Budget 25 tokens
	// fits at most 2 verbatim, forcing AdaptiveCompressor to summarize the
	// rest. The query matches convo[3] best, so relevance order would rank
	// it first; chronological order must still put convo[0] and convo[1]
	// (whichever two survive) before the summary, in their original order.
	convo := []core.Message{
		{Role: core.RoleUser, Content: strings.Repeat("a", 40)},
		{Role: core.RoleAssistant, Content: strings.Repeat("b", 40)},
		{Role: core.RoleUser, Content: strings.Repeat("c", 40)},
		{Role: core.RoleUser, Content: "banana banana banana banana"},
	}
	window, err := e.Assemble(context.Background(), AssembleRequest{
		Query:           "banana banana banana banana",
		Conversation:    convo,
		AvailableTokens: 25,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(window.Messages) < 2 {
		t.Fatalf("expected at least a survivor and a summary, got %d: %+v", len(window.Messages), window.Messages)
	}
	last := window.Messages[len(window.Messages)-1]
	if !strings.Contains(last.ContentString(), "Summarized Context") {
		t.Errorf("last message = %q, want the compressed-overflow summary last", last.ContentString())
	}
	survivors := window.Messages[:len(window.Messages)-1]
	// Whatever survived must appear in the same relative order convo had
	// them in, not relevance order (which would put convo[3] first).
	lastSeenIdx := -1
	for _, s := range survivors {
		idx := -1
		for i, c := range convo {
			if c.ContentString() == s.ContentString() {
				idx = i
				break
			}
		}
		if idx == -1 {
			t.Fatalf("survivor %q not found in original convo", s.ContentString())
		}
		if idx <= lastSeenIdx {
			t.Errorf("survivor at convo index %d appeared out of chronological order (last seen index %d)", idx, lastSeenIdx)
		}
		lastSeenIdx = idx
	}
}

// TestAssembleOverflowCompressionUsesTheRealClientAfterSetClient is the
// regression test for wiring Engine.SetClient into the summarizer AdaptiveCompressor
// wraps: before this fix, Assemble's own overflow-compression step was
// permanently extractive-only in production (the summarizer's client stayed
// nil regardless of SetClient), even though it looked LLM-backed. Same
// overflow scenario as TestAssembleKeepsSurvivorsChronologicalAndSummaryLast,
// but with a real client wired via SetClient — the fake client must actually
// receive the summarization request.
func TestAssembleOverflowCompressionUsesTheRealClientAfterSetClient(t *testing.T) {
	e := NewEngine(nil)
	fake := &compressFakeClient{reply: "the LLM's own summary"}
	e.SetClient(fake, "model")

	convo := []core.Message{
		{Role: core.RoleUser, Content: strings.Repeat("a", 40)},
		{Role: core.RoleAssistant, Content: strings.Repeat("b", 40)},
		{Role: core.RoleUser, Content: strings.Repeat("c", 40)},
		{Role: core.RoleUser, Content: "banana banana banana banana"},
	}
	window, err := e.Assemble(context.Background(), AssembleRequest{
		Query:           "banana banana banana banana",
		Conversation:    convo,
		AvailableTokens: 25,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if fake.got == nil {
		t.Fatal("overflow compression did not reach the client wired via SetClient — still extractive-only")
	}
	last := window.Messages[len(window.Messages)-1]
	if !strings.Contains(last.ContentString(), "the LLM's own summary") {
		t.Errorf("last message = %q, want it to contain the LLM's summary, not the extractive fallback", last.ContentString())
	}
}

// TestAssembleIncludesInjectionsWhenTheyFit also pins the ordering: an
// injection that survives alongside the conversation lands AFTER it
// (injections are appended to convo before ranking, so they sort as the most
// recent turn — see the Assemble comment) — that ordering is load-bearing for
// how callers like executeDirectNoTools expect a recall block to read
// relative to the conversation, so a regression here should fail loudly.
func TestAssembleIncludesInjectionsWhenTheyFit(t *testing.T) {
	e := NewEngine(nil)
	window, err := e.Assemble(context.Background(), AssembleRequest{
		Query:           "capital of France",
		Conversation:    []core.Message{{Role: core.RoleUser, Content: "let's talk about bananas"}},
		Injections:      []core.Message{{Role: core.RoleSystem, Content: "## Relevant Past Context\nParis is the capital of France."}},
		AvailableTokens: 10000,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	convoIdx, injectIdx := -1, -1
	for i, m := range window.Messages {
		if m.ContentString() == "let's talk about bananas" {
			convoIdx = i
		}
		if strings.Contains(m.ContentString(), "Paris is the capital of France") {
			injectIdx = i
		}
	}
	if injectIdx < 0 {
		t.Fatalf("expected the injection to appear in the assembled window, got: %+v", window.Messages)
	}
	if convoIdx < 0 {
		t.Fatalf("expected the conversation message to appear in the assembled window, got: %+v", window.Messages)
	}
	if injectIdx < convoIdx {
		t.Errorf("expected the injection (index %d) to land after the conversation message (index %d), got: %+v", injectIdx, convoIdx, window.Messages)
	}
}

// TestAssembleInjectionsCompeteForBudget proves Injections are NOT
// unconditionally kept verbatim like a real system message: given a token
// budget too small for everything, a low-relevance injection loses out to
// higher-relevance conversation and gets folded into the overflow summary
// (AdaptiveCompressor.Compress) instead of surviving as its own full
// message — exactly like any other losing candidate, not specially exempted.
func TestAssembleInjectionsCompeteForBudget(t *testing.T) {
	e := NewEngine(nil)
	filler := strings.Repeat("irrelevant filler about spacecraft engineering ", 20)
	window, err := e.Assemble(context.Background(), AssembleRequest{
		Query: "banana banana banana banana",
		Conversation: []core.Message{
			{Role: core.RoleUser, Content: "banana banana banana banana"},
		},
		// Long, entirely irrelevant to the query, and NOT capped by the
		// caller — Assemble's shared budget is what has to constrain it now.
		Injections:      []core.Message{{Role: core.RoleSystem, Content: filler}},
		AvailableTokens: 15, // fits only the highest-relevance survivor
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for _, m := range window.Messages {
		if m.ContentString() == filler {
			t.Errorf("expected the low-relevance injection to lose out under a tight budget (survive only inside the overflow summary, if at all), but it survived verbatim as its own message: %+v", window.Messages)
		}
	}
}

func TestAssembleSystemPromptAndSystemMessagesComeFirst(t *testing.T) {
	e := NewEngine(nil)
	convo := []core.Message{
		{Role: core.RoleSystem, Content: "pinned system note"},
		{Role: core.RoleUser, Content: "hello"},
	}
	window, err := e.Assemble(context.Background(), AssembleRequest{
		Query:           "hello",
		Conversation:    convo,
		SystemPrompt:    "you are a helpful assistant",
		AvailableTokens: 10000,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(window.Messages) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(window.Messages), window.Messages)
	}
	if window.Messages[0].Role != core.RoleSystem || window.Messages[0].ContentString() != "pinned system note" {
		t.Errorf("Messages[0] = %+v, want the pinned system message first", window.Messages[0])
	}
	if window.Messages[1].Role != core.RoleSystem || window.Messages[1].ContentString() != "you are a helpful assistant" {
		t.Errorf("Messages[1] = %+v, want the SystemPrompt second", window.Messages[1])
	}
	if window.Messages[2].ContentString() != "hello" {
		t.Errorf("Messages[2] = %+v, want the user turn last", window.Messages[2])
	}
}
