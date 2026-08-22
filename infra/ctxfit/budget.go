package ctxfit

import (
	"strings"

	"github.com/darkcode/infra/core"
)

// ============================================================================
// TOKEN BUDGET — Proactive context-window management for OpenAI-compatible
// and local LLM providers.
//
// Instead of reacting to "context deadline exceeded" errors AFTER the fact,
// the budget calculator computes how much space is available BEFORE each LLM
// call. If the assembled context exceeds the budget, compression is triggered
// preemptively.
//
// This works for any provider registered in the provider catalogue
// (config/providers.go) — both cloud (OpenAI, Anthropic, Gemini, ...) and
// local (Ollama, LM Studio) endpoints. For custom OpenAI-compatible
// endpoints, the user sets context_length in their config.
// ============================================================================

// DefaultContextWindow is used when no context window is known for the model.
const DefaultContextWindow = 128000

// ResponseReservePercent is the percentage of the context window reserved for
// the LLM's response. 40% leaves generous room for tool-using agents.
const ResponseReservePercent = 40

// SystemReserveTokens is the fixed token reservation for the system prompt.
const SystemReserveTokens = 600

// TokensPerToolSchema is the estimated tokens consumed by each tool schema
// in the `tools` field of the completion request.
const TokensPerToolSchema = 120

// EstimateTokens estimates the token count for a slice of messages, adding the
// per-message and per-tool-call overhead a bare string estimate cannot see.
func EstimateTokens(messages []core.Message) int {
	total := 0
	for _, msg := range messages {
		total += EstimateStringTokens(msg.ContentString())
		for _, tc := range msg.ToolCalls {
			total += EstimateStringTokens(tc.Function.Arguments)
			total += EstimateStringTokens(tc.Function.Name) + 4 // function call overhead
		}
		// Per-message overhead (role token, separators)
		total += 4
	}
	return total
}

// EstimateStringTokens estimates the token count for a single string.
//
// This used to count whitespace-separated words and scale by 1.3, which
// matched English prose but ran roughly 29% low on source code — code has few
// spaces for its length, so the estimate hit the conservative len/6 floor.
// Reading low is the harmful direction here: the result is a context budget,
// and an under-estimate packs too much into the window for the provider to
// accept. It now defers to core.EstimateTokens, the single estimator every
// package shares.
func EstimateStringTokens(s string) int { return core.EstimateTokens(s) }

// FitToWindow is the deterministic, no-LLM backstop that GUARANTEES a message
// slice fits window−reserve tokens before it is sent to any model. It is the
// single choke point every dispatch family calls immediately before building
// the request, so no code path can hand an over-long prompt to a client —
// whichever model the router picked, the prompt is fitted to THAT model's
// effective window (from ModelInfo().Context).
//
// The LLM Compressor still runs earlier on STM growth for a semantic summary;
// this function is the hard final guarantee for the cases the compressor
// didn't catch (a single giant turn, a large tool result, tokenizer drift).
// It is pure and synchronous so it can be called cheaply everywhere.
//
// Invariants: the system prompt (a leading role=="system" message) and the
// most recent user turn are NEVER dropped — they are the irreducible request.
// Everything between them is shed oldest-first, then the largest survivor is
// middle-truncated, until the estimate fits.
//
// window<=0 means "unknown" (a client that can't report its size): the caller
// should pass a sensible fallback (cfg.ContextLength) rather than 0, but if 0
// slips through, FitToWindow returns messages unchanged rather than destroying
// context on a bad signal.
func FitToWindow(messages []core.Message, window, reserve int) []core.Message {
	if window <= 0 {
		return messages
	}
	budget := window - reserve
	if budget < 256 {
		budget = 256 // never fit to nothing; a tiny window still needs the request
	}
	if EstimateTokens(messages) <= budget || len(messages) == 0 {
		return messages
	}

	// Identify the protected anchors: a leading system message and the trailing
	// exchange (the current turn plus, when it is a tool response, the
	// assistant message that asked for it).
	//
	// Protecting the last message ALONE was wrong, and produced a real failure.
	// A tool response is not self-contained: shedding the assistant that issued
	// its call leaves the response an orphan, repairToolPairs correctly deletes
	// it, and what survives is the system prompt by itself. Gemini folds system
	// messages into systemInstruction, so the request goes out with an empty
	// contents array and comes back "GenerateContentRequest.contents: contents
	// is not specified" — a 400 on a worker that was making normal progress,
	// triggered by nothing more than one oversized tool result.
	sysIdx := -1
	if len(messages) > 0 && messages[0].Role == core.RoleSystem {
		sysIdx = 0
	}
	protected := trailingExchange(messages)

	kept := make([]bool, len(messages))
	for i := range kept {
		kept[i] = true
	}
	for i := 0; i < len(messages); i++ {
		if i == sysIdx || protected[i] {
			continue
		}
		if estimateKept(messages, kept) <= budget {
			break
		}
		kept[i] = false
	}
	if estimateKept(messages, kept) <= budget {
		return repairToolPairs(collectKept(messages, kept))
	}

	out := repairToolPairs(collectKept(messages, kept))
	for EstimateTokens(out) > budget {
		bi, bTok := -1, 0
		for i := range out {
			t := EstimateStringTokens(out[i].ContentString())
			if t > bTok {
				bTok, bi = t, i
			}
		}
		if bi < 0 || bTok <= 40 {
			break // nothing left worth truncating; accept the floor
		}
		out[bi].Content = truncateMiddle(out[bi].ContentString(), bTok/2)
	}
	return ensureConversational(out, messages)
}

// trailingExchange marks the messages that make up the final turn: the last
// message, and — when that is a tool response — the assistant that issued its
// call plus every sibling response to that same assistant.
//
// The siblings matter because a single assistant turn can request several tools
// at once. Keeping the assistant and only one of its responses leaves the rest
// unanswered, which Gemini rejects just as firmly as an orphaned response.
func trailingExchange(messages []core.Message) []bool {
	protected := make([]bool, len(messages))
	if len(messages) == 0 {
		return protected
	}
	last := len(messages) - 1
	protected[last] = true

	if messages[last].Role != core.RoleTool || messages[last].ToolCallID == "" {
		return protected
	}
	// Find the assistant that asked for this response.
	askerIdx := -1
	for i := last - 1; i >= 0; i-- {
		for _, tc := range messages[i].ToolCalls {
			if tc.ID == messages[last].ToolCallID {
				askerIdx = i
				break
			}
		}
		if askerIdx >= 0 {
			break
		}
	}
	if askerIdx < 0 {
		return protected
	}
	protected[askerIdx] = true

	// Every response answering that same assistant.
	ids := map[string]bool{}
	for _, tc := range messages[askerIdx].ToolCalls {
		ids[tc.ID] = true
	}
	for i := askerIdx + 1; i < len(messages); i++ {
		if messages[i].Role == core.RoleTool && ids[messages[i].ToolCallID] {
			protected[i] = true
		}
	}
	return protected
}

// ensureConversational is the post-condition: a fitted request must carry at
// least one non-system message.
//
// Anchor protection above prevents the known way of losing them all, but this
// is the guarantee rather than the mechanism — a request with nothing but a
// system prompt is invalid on Gemini and meaningless everywhere else, so it is
// worth checking rather than assuming. Falling back to a heavily truncated
// last user turn keeps the request valid; returning an empty conversation never
// can be.
func ensureConversational(out, original []core.Message) []core.Message {
	for _, m := range out {
		if m.Role != core.RoleSystem {
			return out
		}
	}
	// Prefer the most recent user turn: it is the only message guaranteed to
	// stand alone, unlike an assistant tool call or a tool response.
	for i := len(original) - 1; i >= 0; i-- {
		if original[i].Role == core.RoleUser {
			m := original[i]
			m.Content = truncateMiddle(m.ContentString(), 512)
			return append(out, m)
		}
	}
	return out
}

// estimateKept sums the token estimate of only the messages still flagged kept.
func estimateKept(messages []core.Message, kept []bool) int {
	total := 0
	for i, m := range messages {
		if kept[i] {
			total += EstimateStringTokens(m.ContentString()) + 4
		}
	}
	return total
}

func collectKept(messages []core.Message, kept []bool) []core.Message {
	out := make([]core.Message, 0, len(messages))
	for i, m := range messages {
		if kept[i] {
			out = append(out, m)
		}
	}
	return out
}

// truncateMiddle keeps the head and tail of s, replacing the middle with an
// elision marker, so the result is roughly targetTokens. Head+tail preserves
// the message's opening intent and closing detail — better than a hard tail
// cut for both instructions and tool output.
func truncateMiddle(s string, targetTokens int) string {
	if targetTokens < 20 {
		targetTokens = 20
	}
	// ~4 chars/token is fine here; we re-estimate in the caller's loop.
	targetChars := targetTokens * 4
	if len(s) <= targetChars {
		return s
	}
	const marker = "\n…[truncated to fit context window]…\n"
	half := (targetChars - len(marker)) / 2
	if half < 1 {
		return s[:targetChars]
	}
	return s[:half] + marker + s[len(s)-half:]
}

// FitClient is the one-liner every dispatch family calls right before it
// builds a request: it fits messages to the RECEIVING client's effective
// window (ModelInfo().Context — the governor's NCtx/NParallel for a local
// model, the catalogue window for a cloud one), reserving room for the
// response and the tool schemas. Falls back to cfgContextLength then the
// package default when a client can't report its window, so cfg.ContextLength
// finally has a real consumer.
func FitClient(messages []core.Message, client core.LLMClient, cfgContextLength, toolCount int) []core.Message {
	window := 0
	if client != nil {
		window = client.ModelInfo().Context
	}
	if window <= 0 {
		window = cfgContextLength
	}
	if window <= 0 {
		window = DefaultContextWindow
	}
	return FitToWindow(messages, window, reserveFor(window, toolCount))
}

// reserveFor computes the token reservation FitClient subtracts from window:
// room for the response, the tool schemas, and the system prompt. Shared
// with UsableBudget so the two stay in agreement by construction.
func reserveFor(window, toolCount int) int {
	return window*ResponseReservePercent/100 + toolCount*TokensPerToolSchema + SystemReserveTokens
}

// UsableBudget returns how many tokens are actually available for message
// content once FitClient's own reserves are subtracted from window — the
// same arithmetic FitClient applies internally, exposed so an upstream
// caller building a request BEFORE it reaches FitClient (e.g.
// ctxengine.Engine.Assemble, which trims by relevance rather than recency)
// can budget against the number that will actually survive the wire.
//
// Without this, an upstream budget of the raw window routinely comes in
// well above what FitClient will actually allow (window minus ~40% response
// reserve minus a 600-token system reserve minus tool schemas), so the
// upstream trim never engages — everything passes through untouched — and
// FitClient ends up doing ALL the trimming itself, by its own oldest-first
// recency rule. That silently discards whatever relevance-based selection
// the upstream caller made: its ranked survivors get re-shed by recency
// before they ever reach the model. Floors at 256, matching FitToWindow's
// own floor, so a tiny or misreported window still leaves room for a
// request.
func UsableBudget(window, toolCount int) int {
	if window <= 0 {
		window = DefaultContextWindow
	}
	budget := window - reserveFor(window, toolCount)
	if budget < 256 {
		budget = 256
	}
	return budget
}

// FitsInBudget checks whether the given messages fit within the token budget.
func FitsInBudget(messages []core.Message, budget int) bool {
	return EstimateTokens(messages) <= budget
}

// repairToolPairs drops tool-call/tool-response pairs that shedding has broken
// apart.
//
// FitToWindow removes whole messages oldest-first, which is fine for prose and
// wrong for tool use: an assistant message carrying tool_calls and the role=tool
// messages answering it are one indivisible exchange. Drop the assistant and the
// responses become orphans; drop a response and the call is left unanswered.
//
// OpenAI tolerates both. Gemini's OpenAI-compatible endpoint does not — it
// rejects the request outright with "function call turn must come immediately
// after a user turn or a function response turn", and the failure surfaces at
// the API boundary with nothing pointing back at the trimmer that caused it.
// So the invariant is enforced here: a tool exchange is kept whole or not at
// all.
func repairToolPairs(msgs []core.Message) []core.Message {
	// Which calls still have an assistant asking for them, and which have a
	// response.
	asked := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			asked[tc.ID] = true
		}
		if m.Role == core.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}

	out := make([]core.Message, 0, len(msgs))
	for _, m := range msgs {
		// A response whose call was shed is an orphan.
		if m.Role == core.RoleTool {
			if m.ToolCallID == "" || !asked[m.ToolCallID] {
				continue
			}
			out = append(out, m)
			continue
		}
		// An assistant whose responses were shed leaves a call hanging. Keep
		// the message when it carries other content, minus the calls; drop it
		// when the calls were all it had.
		if len(m.ToolCalls) > 0 {
			complete := true
			for _, tc := range m.ToolCalls {
				if !answered[tc.ID] {
					complete = false
					break
				}
			}
			if !complete {
				if strings.TrimSpace(m.ContentString()) == "" {
					continue
				}
				m.ToolCalls = nil
			}
		}
		out = append(out, m)
	}
	return out
}
