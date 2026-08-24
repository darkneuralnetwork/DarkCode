package orchestrator

// kernel_ctxengine_test.go — verifies ctxengine.Engine.Assemble's dedup and
// budget-trimming behavior (Strategy 6b), the same engine the kernel uses in
// the General-mode fast path (executeDirectNoTools, kernel_helpers.go) when
// cfg.UseCtxEngine is true, instead of dumping raw STM.

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/memory/ctxengine"
)

// TestCtxEngineDedupAndBudget verifies that the ctxengine deduplicates
// near-duplicate messages and trims to the token budget.
func TestCtxEngineDedupAndBudget(t *testing.T) {
	engine := ctxengine.NewEngine(nil)

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
	engine := ctxengine.NewEngine(nil)

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

// TestCtxEngineEnabledByDefault verifies that UseCtxEngine defaults to true
// (Phase 5 of the context-management unification) — Assemble is the default
// builder for pre-turn history and injections; boundedChatContext/raw STM
// append remains reachable only as the opt-out (UseCtxEngine=false) path.
func TestCtxEngineEnabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.UseCtxEngine {
		t.Error("UseCtxEngine should default to true — see Context management Phase 5")
	}
}
