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
