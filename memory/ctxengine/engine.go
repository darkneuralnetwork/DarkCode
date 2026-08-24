package ctxengine

import (
	"context"
	"sort"
	"sync"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/ctxfit"
)

// AssembleRequest holds all inputs needed to assemble a context window.
type AssembleRequest struct {
	Query        string
	Conversation []core.Message
	SystemPrompt string
	// Injections are caller-supplied context — a recall/RAG block, project
	// notes, anything that isn't part of the conversation itself but needs to
	// reach the model. They are budgeted ALONGSIDE Conversation in the same
	// relevance-ranked pool, rather than unconditionally kept the way a real
	// system message is: when everything doesn't fit, an injection can lose
	// to higher-relevance conversation instead of always winning the space a
	// caller that pre-concatenated it into the system prompt would have given
	// it. This is a second line of defense, not a replacement for the
	// caller's own cap — a caller should still bound what it hands in here
	// (getRecallBlock and BuildContextQuery both do); Assemble's budget is
	// what keeps a still-large, still-relevant injection from silently
	// crowding out the conversation it's competing with, not a license to
	// hand in unbounded content. Injections retain their own Role (typically
	// RoleSystem) in the output.
	Injections      []core.Message
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
// adaptively compress the overflow into a summary → restore the surviving
// messages to their original chronological order. The system prompt is
// always preserved at the head of the result.
//
// Callers: this is for PRE-TURN conversational history (prior STM handed
// into a fresh loop.Run, or Chat's prior turns) — content with no structural
// adjacency requirements, where relevance-ranking and reordering is safe.
// It is deliberately NOT used for a live, in-flight tool-calling loop's own
// growing message list: an assistant tool_call must stay immediately
// followed by its role:tool response or the provider rejects the request
// outright, and neither the ranker nor the deduplicator here are aware of
// that pairing. ctxfit.FitClient (called at dispatch time, inside
// modelport.CompleteWith) is what those call sites use instead — see
// kernel/agents/subagent.go.
func (e *Engine) Assemble(ctx context.Context, req AssembleRequest) (*ContextWindow, error) {
	if req.AvailableTokens <= 0 {
		req.AvailableTokens = 32000 // sensible default
	}

	// Step 1: Deduplicate (exact + near-dup). Only req.Conversation's own
	// system messages get the unconditionally-kept treatment below —
	// req.Injections are deduplicated separately (against each other, not
	// against the conversation: two overlapping injections are unusual and a
	// cross-pool check isn't worth the complexity for what's a single
	// caller-supplied block in practice today) and always join the ranked
	// pool, even when their Role is RoleSystem. That's what makes them
	// compete for space instead of always winning it.
	msgs := e.deduplicator.Deduplicate(req.Conversation)
	injections := e.deduplicator.Deduplicate(req.Injections)

	// Step 2: Separate the system prompt (always kept) from the conversational
	// messages, then rank conversation+injections together by relevance to
	// the query. order[i] is ranked[i]'s index in convo — kept alongside the
	// ranking so the surviving messages can be restored to convo's original
	// order below, instead of left in relevance order (which reads as a
	// shuffled transcript to the model, particularly bad for a "continue"
	// follow-up). Injections sort as if they were the most recent turn (they
	// were appended last, before ranking) when they survive — appropriate for
	// current-turn context like a recall block.
	var system []core.Message
	var convo []core.Message
	for _, m := range msgs {
		if m.Role == core.RoleSystem {
			system = append(system, m)
		} else {
			convo = append(convo, m)
		}
	}
	convo = append(convo, injections...)
	order := e.ranker.RankIndices(ctx, req.Query, convo)
	ranked := make([]core.Message, len(order))
	for i, idx := range order {
		ranked[i] = convo[idx]
	}

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
	// compressor summarizes the overflow. Both TrimToBudget and Compress keep
	// a strict PREFIX of their ranked input (plus, only for Compress, exactly
	// one synthesized summary message appended at the end covering the
	// dropped tail) — survivorCutoff below is that prefix length, used to
	// split "real" survivors (which get chronologically restored) from the
	// synthesized summary (which doesn't correspond to any convo index, and
	// is left exactly where Compress put it).
	trimmed, err := e.tokenBudget.TrimToBudget(ranked, convoBudget)
	survivorCutoff := len(trimmed)
	if err != nil {
		trimmed = e.compressor.Compress(ctx, ranked, convoBudget)
		// Compress always appends exactly one summary message when it
		// shortens its input (see its own comment) — TrimToBudget's error
		// here guarantees the input didn't already fit, so Compress always
		// takes that branch, never its unchanged-passthrough one.
		survivorCutoff = len(trimmed) - 1
		if survivorCutoff < 0 {
			survivorCutoff = 0
		}
	}
	trimmed = restoreChronological(convo, order, trimmed, survivorCutoff)

	final := make([]core.Message, 0, len(system)+len(trimmed)+1)
	final = append(final, system...)
	if req.SystemPrompt != "" {
		final = append(final, core.Message{Role: core.RoleSystem, Content: req.SystemPrompt})
	}
	final = append(final, trimmed...)

	return &ContextWindow{Messages: final}, nil
}

// restoreChronological re-sorts the first cutoff messages of trimmed — the
// relevance-ranked survivors, identified via order (order[i] is ranked[i]'s
// index into convo) — back into convo's original order, and leaves anything
// from cutoff onward (a synthesized overflow summary, if present) untouched
// at the end.
func restoreChronological(convo []core.Message, order []int, trimmed []core.Message, cutoff int) []core.Message {
	if cutoff > len(order) {
		cutoff = len(order)
	}
	keptIdx := append([]int(nil), order[:cutoff]...)
	sort.Ints(keptIdx)
	out := make([]core.Message, 0, len(trimmed))
	for _, idx := range keptIdx {
		out = append(out, convo[idx])
	}
	out = append(out, trimmed[cutoff:]...)
	return out
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
// which estimates per-message via tokenBudget.EstimateTokens — the two
// remain unreconciled; a caller using this for its own pre-Compress check
// should treat it as an upper bound on what Assemble will count, not an
// exact match.
func (e *Engine) EstimateTokens(messages []core.Message) int {
	return ctxfit.EstimateTokens(messages)
}
