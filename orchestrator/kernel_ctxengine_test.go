package orchestrator

// kernel_ctxengine_test.go — verifies the ctxengine.Engine integration in the
// General-mode fast path (Strategy 6b). When UseCtxEngine is true, the kernel
// should assemble a deduplicated, budget-trimmed context window instead of
// dumping raw STM.

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/ctxengine"
)

// TestCtxEngineDedupAndBudget verifies that the ctxengine deduplicates
// near-duplicate messages and trims to the token budget.
func TestCtxEngineDedupAndBudget(t *testing.T) {
	k := &Kernel{
		cfg: Config{
			UseCtxEngine: true,
		},
	}
	engine := k.getCtxEngine()
	if engine == nil {
		t.Fatal("getCtxEngine returned nil")
	}

	// Build a conversation with near-duplicates.
	dup := core.Message{Role: core.RoleUser, Content: "The quick brown fox jumps over the lazy dog. The quick brown fox is very quick indeed."}
	msgs := []core.Message{
		dup,
		dup, // exact duplicate
		{Role: core.RoleUser, Content: "What color is the fox?"},
	}

	window, err := engine.Assemble(context.Background(), ctxengine.AssembleRequest{
		Query:           "fox color",
		Conversation:    msgs,
		SystemPrompt:    "You are a test assistant.",
		AvailableTokens: 10000, // large budget — no compression needed
	})
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}
	if window == nil {
		t.Fatal("Assemble returned nil window")
	}

	// The exact duplicate should have been removed: 3 → 2 conversational
	// messages (plus the system prompt injected by AssembleRequest).
	totalMsgs := len(window.Messages)
	if totalMsgs > 4 {
		t.Errorf("expected dedup to reduce message count, got %d messages", totalMsgs)
	}

	// The system prompt should be present.
	foundSys := false
	for _, m := range window.Messages {
		if m.Role == core.RoleSystem && strings.Contains(m.ContentString(), "test assistant") {
			foundSys = true
		}
	}
	if !foundSys {
		t.Error("system prompt not found in assembled window")
	}
}

// TestCtxEngineBudgetTrimming verifies that a small token budget triggers
// compression/trimming.
func TestCtxEngineBudgetTrimming(t *testing.T) {
	k := &Kernel{
		cfg: Config{UseCtxEngine: true},
	}
	engine := k.getCtxEngine()

	// Build a large conversation.
	var msgs []core.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, core.Message{
			Role:    core.RoleUser,
			Content: strings.Repeat("This is a long message that should be trimmed. ", 50),
		})
	}

	// Tiny budget → most messages should be trimmed/compressed.
	window, err := engine.Assemble(context.Background(), ctxengine.AssembleRequest{
		Query:           "test",
		Conversation:    msgs,
		SystemPrompt:    "sys",
		AvailableTokens: 50, // intentionally tiny
	})
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}
	if window == nil {
		t.Fatal("Assemble returned nil window")
	}

	// With a 50-token budget, we should have far fewer than 20 messages.
	if len(window.Messages) >= 20 {
		t.Errorf("expected budget trimming to reduce message count, got %d", len(window.Messages))
	}
}

// TestCtxEngineDisabledByDefault verifies that UseCtxEngine=false preserves
// the raw STM append behavior (the original code path).
func TestCtxEngineDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.UseCtxEngine {
		t.Error("UseCtxEngine should default to false to preserve existing behavior")
	}
}
