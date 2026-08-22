package ctxengine

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

func msg(role core.Role, chars int) core.Message {
	return core.Message{Role: role, Content: strings.Repeat("a", chars)}
}

// TestDeduplicate_PreservesShortRepeatedTurns is the regression test for the
// bug caught reviewing Phase 3 of the context-management unification: two
// SEPARATE "continue" turns (a real, common follow-up in a loop.Run history)
// have identical shingle sets under shingleSet's under-k fallback (a bag of
// single words), so Jaccard scored them as a 1.0 near-duplicate and the
// second one was silently dropped — breaking user/assistant turn alternation
// right before a "continue" follow-up, the exact case
// loopHistoryBudgetTokens/Assemble exists to preserve. minDedupWords fixes
// this by never considering short messages for removal.
func TestDeduplicate_PreservesShortRepeatedTurns(t *testing.T) {
	d := NewDeduplicator()
	msgs := []core.Message{
		{Role: core.RoleUser, Content: "continue"},
		{Role: core.RoleAssistant, Content: "did some work"},
		{Role: core.RoleUser, Content: "continue"},
		{Role: core.RoleAssistant, Content: "did more work"},
	}
	out := d.Deduplicate(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("Deduplicate dropped a short repeated turn: got %d messages, want all %d kept: %+v", len(out), len(msgs), out)
	}
}

// TestDeduplicate_StillDropsLongExactDuplicates is the companion case:
// minDedupWords must not blanket-disable dedup — a genuinely long,
// word-for-word repeated message is still redundant and should still go.
func TestDeduplicate_StillDropsLongExactDuplicates(t *testing.T) {
	d := NewDeduplicator()
	long := "this is a much longer message with plenty of words to carry real shingle signal"
	msgs := []core.Message{
		{Role: core.RoleUser, Content: long},
		{Role: core.RoleAssistant, Content: "ack"},
		{Role: core.RoleUser, Content: long},
	}
	out := d.Deduplicate(msgs)
	if len(out) != 2 {
		t.Fatalf("Deduplicate kept a long exact duplicate: got %d messages, want 2: %+v", len(out), out)
	}
}

// TestAdaptiveCompressor_KeepsHighestRankedMessages exercises the real call
// path: engine.go's Assemble ranks messages most-relevant-first (see
// ContextRanker.Rank) and hands that order straight to Compress on overflow.
// Compress must keep the front (highest-relevance) messages that fit and
// summarize the low-relevance tail — not the reverse.
func TestAdaptiveCompressor_KeepsHighestRankedMessages(t *testing.T) {
	// Rank order: msg0, msg1 are the two most relevant (10 tokens each at
	// 4 chars/token). msg2 is the least relevant but alone is oversized
	// (50 tokens). Budget is 30: msg0+msg1 = 20 fits comfortably; adding
	// msg2 would blow it.
	msgs := []core.Message{
		msg(core.RoleUser, 40),      // rank 1 (most relevant), 10 tokens
		msg(core.RoleAssistant, 40), // rank 2, 10 tokens
		msg(core.RoleUser, 200),     // rank 3 (least relevant), 50 tokens
	}

	c := NewAdaptiveCompressor(nil) // no summarizer: exercises the deterministic fallback
	out := c.Compress(context.Background(), msgs, 30)

	if len(out) < 2 {
		t.Fatalf("expected the two most-relevant messages to survive compression, got %d messages: %+v", len(out), out)
	}
	if out[0].Content != msgs[0].Content || out[1].Content != msgs[1].Content {
		t.Fatalf("expected the two highest-ranked messages kept verbatim in rank order, got: %+v", out)
	}
}

// TestAdaptiveCompressor_SingleOversizedTopMessageIsKeptAnyway covers the
// cutoff<=0 guard: even the single highest-ranked message alone can exceed
// the budget. It must still be kept (deliberate, bounded overflow) rather
// than summarized away — summarizing the one message the caller most wants
// to see would defeat the point of ranking by relevance in the first place.
func TestAdaptiveCompressor_SingleOversizedTopMessageIsKeptAnyway(t *testing.T) {
	msgs := []core.Message{
		msg(core.RoleUser, 200), // 50 tokens, alone over the 30-token limit
		msg(core.RoleUser, 40),  // 10 tokens, would never be reached
	}
	c := NewAdaptiveCompressor(nil)
	out := c.Compress(context.Background(), msgs, 30)

	if len(out) == 0 || out[0].Content != msgs[0].Content {
		t.Fatalf("expected the single most-relevant message kept despite overflowing budget, got: %+v", out)
	}
}
