package llm

import "github.com/darkcode/core"

// ============================================================================
// PROMPT CACHING
//
// The system prompt is by far the largest stable block in an agent request —
// role instructions, project brief, conventions — and it is resent on every
// turn of every task. Providers will serve it from a prefix cache at a
// fraction of the input price, but the two families ask for it differently:
//
//   • OpenAI (and its compatible endpoints) cache automatically. Nothing to
//     send; the only requirement is that the prefix stays byte-identical
//     across calls, which it does because the system message is built once.
//   • Anthropic — directly or through OpenRouter — caches only what is marked
//     with an explicit `cache_control` breakpoint.
//
// So the work here is narrow: mark the system message for the providers that
// need marking, and leave every other request untouched.
// ============================================================================

// explicitCacheProviders need a cache_control breakpoint to cache anything.
var explicitCacheProviders = map[string]bool{"anthropic": true, "openrouter": true}

// outgoing returns the request as it should go on the wire: a shallow copy
// with this client's defaults applied. It never mutates the caller's request,
// which matters because consensus and cascade fan the same request out to
// several providers — a breakpoint added for Anthropic must not follow it to a
// local server that cannot parse content parts.
func (c *Client) outgoing(req *core.CompletionRequest) *core.CompletionRequest {
	out := *req
	if out.ReasoningEffort == "" {
		out.ReasoningEffort = c.Effort
	}
	if explicitCacheProviders[c.Provider] {
		out.Messages = withSystemCacheBreakpoint(out.Messages)
	}
	return &out
}

// withSystemCacheBreakpoint returns msgs with the leading system message
// rewritten as a single text content part carrying an ephemeral cache
// breakpoint. Messages already in content-part form, or a slice with no system
// message, are returned unchanged and uncopied.
func withSystemCacheBreakpoint(msgs []core.Message) []core.Message {
	for i := range msgs {
		if msgs[i].Role != core.RoleSystem {
			continue
		}
		text, ok := msgs[i].Content.(string)
		if !ok || text == "" {
			return msgs // already structured, or nothing to cache
		}
		out := make([]core.Message, len(msgs))
		copy(out, msgs)
		out[i].Content = []core.ContentPart{{
			Type:         "text",
			Text:         text,
			CacheControl: &core.CacheControl{Type: "ephemeral"},
		}}
		return out // only the first system message is the stable prefix
	}
	return msgs
}
