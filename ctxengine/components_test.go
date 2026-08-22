package ctxengine

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

func msg(role core.Role, chars int) core.Message {
	return core.Message{Role: role, Content: strings.Repeat("a", chars)}
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
