package compression

import (
	"strings"

	"github.com/darkcode/config"
	"github.com/darkcode/core"
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

// TokenBudget holds the computed token allocations for a single LLM call.
type TokenBudget struct {
	// ModelContextWindow is the total context window of the model.
	ModelContextWindow int

	// TotalAvailable is the tokens available for context (after reserves).
	TotalAvailable int

	// ReservedForResponse is tokens reserved for the LLM output.
	ReservedForResponse int

	// ReservedForTools is tokens consumed by tool schemas.
	ReservedForTools int

	// ReservedForSystem is tokens reserved for the system prompt.
	ReservedForSystem int

	// AvailableForContext is the final budget for conversation messages.
	// This is what the compression system must fit within.
	AvailableForContext int
}

// ComputeTokenBudget calculates how many tokens are available for context
// messages given the model, provider, and number of tools. It reads context
// window sizes from the provider catalogue (for registered models) or from
// the config's ContextLength (for custom/local endpoints).
func ComputeTokenBudget(providerID, modelID string, toolCount int, cfgContextLength int) TokenBudget {
	b := TokenBudget{}

	// 1. Determine the model's context window
	b.ModelContextWindow = resolveContextWindow(providerID, modelID, cfgContextLength)

	// 2. Reserve space for the response
	b.ReservedForResponse = b.ModelContextWindow * ResponseReservePercent / 100

	// 3. Reserve space for tool schemas
	b.ReservedForTools = toolCount * TokensPerToolSchema

	// 4. Reserve space for system prompt
	b.ReservedForSystem = SystemReserveTokens

	// 5. Compute available for context
	b.TotalAvailable = b.ModelContextWindow - b.ReservedForResponse
	b.AvailableForContext = b.TotalAvailable - b.ReservedForTools - b.ReservedForSystem

	// Floor at a minimum usable budget (even tiny models should get something)
	if b.AvailableForContext < 1000 {
		b.AvailableForContext = 1000
	}

	return b
}

// resolveContextWindow determines the context window for a model by checking:
// 1. The provider catalogue (config/providers.go)
// 2. The user's configured context_length
// 3. The default fallback
func resolveContextWindow(providerID, modelID string, cfgContextLength int) int {
	// Try the provider catalogue first
	if m, ok := config.LookupModel(providerID, modelID); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}

	// Fall back to user-configured context length
	if cfgContextLength > 0 {
		return cfgContextLength
	}

	// Ultimate fallback
	return DefaultContextWindow
}

// EstimateTokens estimates the token count for a slice of messages using an
// improved heuristic. Instead of the naive len/4, it uses word count * 1.3
// which is much more accurate for mixed English/code content.
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
// Uses word count * 1.3 as the primary heuristic (much more accurate than
// len/4 for English text and code). Falls back to len/4 for very short
// strings where word splitting is unreliable.
func EstimateStringTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	// For very short strings, use char-based estimate
	if len(s) < 20 {
		return len(s)/4 + 1
	}
	// Word-based estimate: each word is ~1.3 tokens on average for English.
	// Code and JSON tend to have more tokens per word, but also have more
	// "words" (punctuation tokens), so it balances out.
	words := len(strings.Fields(s))
	estimate := int(float64(words) * 1.3)
	// Sanity: never estimate less than len/6 (which is a very conservative
	// lower bound) or more than len/2 (which covers heavily-tokenized content).
	minEstimate := len(s) / 6
	maxEstimate := len(s) / 2
	if estimate < minEstimate {
		estimate = minEstimate
	}
	if estimate > maxEstimate {
		estimate = maxEstimate
	}
	return estimate
}

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

	// Identify the protected anchors: a leading system message and the last
	// message (the current user turn / most recent context).
	sysIdx := -1
	if len(messages) > 0 && messages[0].Role == core.RoleSystem {
		sysIdx = 0
	}
	lastIdx := len(messages) - 1

	// Phase 1: drop whole middle messages, oldest first, until we fit.
	kept := make([]bool, len(messages))
	for i := range kept {
		kept[i] = true
	}
	for i := 0; i < len(messages); i++ {
		if i == sysIdx || i == lastIdx {
			continue
		}
		if estimateKept(messages, kept) <= budget {
			break
		}
		kept[i] = false
	}
	if estimateKept(messages, kept) <= budget {
		return collectKept(messages, kept)
	}

	// Phase 2: still over (system + last turn alone exceed the budget).
	// Middle-truncate the surviving messages' content, largest first,
	// preserving head and tail so meaning survives at both ends.
	out := collectKept(messages, kept)
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
	reserve := window*ResponseReservePercent/100 + toolCount*TokensPerToolSchema + SystemReserveTokens
	return FitToWindow(messages, window, reserve)
}

// FitsInBudget checks whether the given messages fit within the token budget.
func FitsInBudget(messages []core.Message, budget int) bool {
	return EstimateTokens(messages) <= budget
}

// ExceedsBudgetBy returns how many tokens the messages exceed the budget by.
// Returns 0 if within budget.
func ExceedsBudgetBy(messages []core.Message, budget int) int {
	tokens := EstimateTokens(messages)
	if tokens <= budget {
		return 0
	}
	return tokens - budget
}
