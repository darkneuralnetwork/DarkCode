package ctxengine

import (
	"context"
	"sync"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/ctxfit"
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

// Engine manages intelligent context assembly for LLM prompts. It is also the
// home of LLM-backed context compression (Compress, Summarize — see
// compress.go): the two used to live in kernel/compression's Compressor,
// which could never route through modelport because modelport itself depends
// on infra/ctxfit for window-fitting, and ctxfit's predecessor package
// (kernel/compression) sat upstream of modelport in the import graph. Engine
// already depended on kernel/modelport (for IncrementalSummarizer), so this
// is where compression could actually become a real CompleteWith caller
// instead of only tagging its context for the call log.
type Engine struct {
	summarizer   *IncrementalSummarizer
	ranker       *ContextRanker
	deduplicator *Deduplicator
	tokenBudget  *TokenBudgetManager
	compressor   *AdaptiveCompressor

	mu       sync.Mutex
	client   core.LLMClient
	model    string
	router   core.ModelRouter
	useLocal bool
}

// NewEngine creates an engine. Pass an optional fast-tier LLM client to enable
// LLM-backed summarization; nil uses the deterministic extractive fallback.
//
// This does NOT also wire Compress/Summarize (compress.go) onto the same
// client — call SetClient separately for that. Keeping the two assignments
// apart is deliberate: a caller that wants Compress/Summarize on a
// hot-swappable client (SetClient is what ReloadModels calls) but doesn't
// want summarizer.llm silently pointing at a stale client from construction
// should pass nil here and call SetClient once, not pass the same client to
// both and have two copies drift apart on the next swap.
func NewEngine(llm core.LLMClient) *Engine {
	summarizer := NewIncrementalSummarizer(llm)
	return &Engine{
		summarizer:   summarizer,
		ranker:       NewContextRanker(),
		deduplicator: NewDeduplicator(),
		tokenBudget:  NewTokenBudgetManager(),
		compressor:   NewAdaptiveCompressor(summarizer),
		useLocal:     true, // matches the old Compressor's default: prefer local to save API cost
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

// SummarizeBlock is a convenience wrapper exposing the incremental summarizer
// for a block of messages. Named distinctly from Summarize (compress.go),
// which produces a narrative briefing of arbitrary text — the two solve
// different problems and previously couldn't coexist as methods with the
// same name.
func (e *Engine) SummarizeBlock(ctx context.Context, msgs []core.Message) core.Message {
	return e.summarizer.Summarize(ctx, msgs)
}

// EstimateTokens estimates the token cost of a full message slice, e.g. for a
// caller's own budget check before calling Compress. Uses the deterministic
// fit layer's estimator (infra/ctxfit), which accounts for per-message and
// tool-call overhead a bare content estimate misses. NOTE: this is a
// different (more accurate) count than Assemble's own internal budgeting,
// which estimates per-message via tokenBudget.EstimateTokens — reconciling
// the two is Phase 3 work, not done here.
func (e *Engine) EstimateTokens(messages []core.Message) int {
	return ctxfit.EstimateTokens(messages)
}
