package ctxengine

import (
	"context"

	"github.com/darkcode/core"
)

// AssembleRequest holds all inputs needed to assemble a context window.
type AssembleRequest struct {
	Query           string
	Conversation    []core.Message
	SystemPrompt    string
	AvailableTokens int
}

// ContextWindow represents the final trimmed and compressed context.
type ContextWindow struct {
	Messages []core.Message
}

// Engine manages intelligent context assembly for LLM prompts.
type Engine struct {
	summarizer   *IncrementalSummarizer
	ranker       *ContextRanker
	deduplicator *Deduplicator
	tokenBudget  *TokenBudgetManager
	compressor   *AdaptiveCompressor
}

// NewEngine creates an engine. Pass an optional fast-tier LLM client to enable
// LLM-backed summarization; nil uses the deterministic extractive fallback.
func NewEngine(llm core.LLMClient) *Engine {
	summarizer := NewIncrementalSummarizer(llm)
	return &Engine{
		summarizer:   summarizer,
		ranker:       NewContextRanker(),
		deduplicator: NewDeduplicator(),
		tokenBudget:  NewTokenBudgetManager(),
		compressor:   NewAdaptiveCompressor(summarizer),
	}
}

// Assemble builds the optimal context window for a request.
//
// Pipeline: deduplicate → rank by query relevance → trim to token budget →
// adaptively compress the overflow into a summary. The system prompt is
// always preserved at the head of the result.
func (e *Engine) Assemble(ctx context.Context, req AssembleRequest) (*ContextWindow, error) {
	if req.AvailableTokens <= 0 {
		req.AvailableTokens = 32000 // sensible default
	}

	// Step 1: Deduplicate (exact + near-dup).
	msgs := e.deduplicator.Deduplicate(req.Conversation)

	// Step 2: Separate the system prompt (always kept) from the conversational
	// messages, then rank the conversation by relevance to the query.
	var system []core.Message
	var convo []core.Message
	for _, m := range msgs {
		if m.Role == core.RoleSystem {
			system = append(system, m)
		} else {
			convo = append(convo, m)
		}
	}
	ranked := e.ranker.Rank(ctx, req.Query, convo)

	// Step 3: Reserve tokens for the system prompt + the query itself.
	sysTokens := 0
	for _, m := range system {
		sysTokens += e.tokenBudget.EstimateTokens(m)
	}
	queryTokens := core.EstimateTokens(req.Query)
	convoBudget := req.AvailableTokens - sysTokens - queryTokens
	if convoBudget < 0 {
		convoBudget = req.AvailableTokens / 2
	}

	// Step 4: Trim to budget. If the ranked set doesn't fit, the adaptive
	// compressor summarizes the overflow.
	trimmed, err := e.tokenBudget.TrimToBudget(ranked, convoBudget)
	if err != nil {
		trimmed = e.compressor.Compress(ctx, ranked, convoBudget)
	}

	// Final order: system prompt → the trimmed conversation in ranker order.
	//
	// This is NOT chronological. The ranker returns most-relevant-first and we
	// present that order unchanged. A chronologicalSort helper used to be
	// called here; it returned its input untouched while its own comment
	// explained it had decided not to sort, so the claimed re-ordering never
	// happened. Restoring chronological order is a real (and probably correct)
	// change to make — it wants message timestamps, which core.Message does not
	// carry today — but it should be made deliberately and measured, not
	// implied by a no-op.
	final := make([]core.Message, 0, len(system)+len(trimmed)+1)
	final = append(final, system...)
	if req.SystemPrompt != "" {
		final = append(final, core.Message{Role: core.RoleSystem, Content: req.SystemPrompt})
	}
	final = append(final, trimmed...)

	return &ContextWindow{Messages: final}, nil
}

// Summarize is a convenience wrapper exposing the summarizer.
func (e *Engine) Summarize(ctx context.Context, msgs []core.Message) core.Message {
	return e.summarizer.Summarize(ctx, msgs)
}

// EstimateTokens exposes the token estimator for callers (e.g. the kernel's
// budget check).
func (e *Engine) EstimateTokens(m core.Message) int {
	return e.tokenBudget.EstimateTokens(m)
}
