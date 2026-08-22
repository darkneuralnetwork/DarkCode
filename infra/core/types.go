package core

import (
	"context"
	"encoding/json"
)

// Role represents the role of a message sender.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall represents a single function/tool call requested by the LLM.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
	// ExtraContent is provider state that must be echoed back on the next
	// request. Absent for every provider that does not use it, so nothing is
	// sent to an endpoint that would reject the field. See ToolCallExtra.
	ExtraContent *ToolCallExtra `json:"extra_content,omitempty"`
}

// FunctionCall is the inner function name + arguments of a ToolCall.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string of arguments
}

// ToolCallExtra carries provider-specific data attached to a tool call that has
// to survive a round trip.
//
// # WHY THIS EXISTS
//
// Gemini 3 returns an opaque thought signature with every function call and
// REFUSES the next request if the signature does not come back with it:
//
//	400 INVALID_ARGUMENT — Function call is missing a thought_signature in
//	functionCall parts.
//
// So a tool call is not just a name and arguments any more; part of it is
// provider state the client has to carry. Dropping it does not degrade the
// answer, it fails the turn — which is exactly what happened here: the agentic
// loop died on iteration 2 of every tool-using task, on the model this project
// is most often run against.
//
// The field lives on ToolCall rather than FunctionCall because that is where
// the wire puts it. An earlier attempt declared it inside FunctionCall, which
// parsed nothing, sent nothing back, and looked correct.
type ToolCallExtra struct {
	Google *GoogleToolExtra `json:"google,omitempty"`
}

// GoogleToolExtra is the Google branch of extra_content.
type GoogleToolExtra struct {
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// Signature returns the thought signature, or "" when there is none. Nil-safe
// so call sites do not each repeat two pointer checks.
func (e *ToolCallExtra) Signature() string {
	if e == nil || e.Google == nil {
		return ""
	}
	return e.Google.ThoughtSignature
}

// WithSignature builds the extra_content a Gemini tool call must echo back.
func WithSignature(sig string) *ToolCallExtra {
	if sig == "" {
		return nil
	}
	return &ToolCallExtra{Google: &GoogleToolExtra{ThoughtSignature: sig}}
}

// Message represents a single message in the conversation history.
type Message struct {
	Role       Role        `json:"role"`
	Content    interface{} `json:"content,omitempty"` // string or []ContentPart
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"` // for role=tool messages
	Name       string      `json:"name,omitempty"`         // tool name for role=tool
}

// ContentPart represents a multi-part message content (for vision, etc.).
type ContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
	// CacheControl marks this part as a prompt-cache breakpoint. Anthropic
	// (directly or via OpenRouter) caches only what is explicitly marked;
	// other providers ignore the field, so it is safe to leave unset.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl is a prompt-cache breakpoint. Only "ephemeral" is defined.
type CacheControl struct {
	Type string `json:"type"`
}

type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ContentString safely extracts the text content from a Message.
func (m *Message) ContentString() string {
	if m.Content == nil {
		return ""
	}
	switch v := m.Content.(type) {
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		var parts []ContentPart
		if err := json.Unmarshal(b, &parts); err != nil {
			return string(b)
		}
		var result string
		for _, p := range parts {
			if p.Text != "" {
				result += p.Text
			}
		}
		return result
	}
}

// SetContent sets the content from a string.
func (m *Message) SetContent(s string) {
	m.Content = s
}

// ContextKey is a custom type for context values to avoid collisions.
type ContextKey string

// WorkspaceKey is the key used to store the active workspace path in the context.
const WorkspaceKey ContextKey = "workspace"
const ProjectKey ContextKey = "project"

// WorkspaceFrom returns the active workspace path carried by ctx, or "" when
// none is set. It exists as an accessor rather than an inline type assertion
// because forgetting to read it is silent: a caller that passes "" where a
// workspace was meant gets code that still runs, just against the process's
// working directory instead of the user's project.
func WorkspaceFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ws, _ := ctx.Value(WorkspaceKey).(string)
	return ws
}

// ReadOnlyToolsKey marks a Chat (read-only) request: only read-only tools are
// offered to the model and mutating tools are refused. Shared by the tool
// registry (enforcement) and the ReAct loop (schema selection).
const ReadOnlyToolsKey ContextKey = "readonly_tools"

// IsReadOnlyTools reports whether ctx carries the read-only tools policy.
func IsReadOnlyTools(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ReadOnlyToolsKey).(bool)
	return v
}

// ReadOnlyReasonKey explains WHY a request is read-only, so the refusal a tool
// returns is actionable. There are two unrelated reasons — the user picked
// Chat mode, or a sub-agent's role has no write authority — and telling a
// scoped research agent to "switch to Build mode" sends it looking for a
// control it does not have.
const ReadOnlyReasonKey ContextKey = "readonly_reason"

// ReadOnlyReason returns the explanation for ctx's read-only policy, or "" when
// none was supplied.
func ReadOnlyReason(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(ReadOnlyReasonKey).(string)
	return s
}
