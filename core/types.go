package core

import "encoding/json"

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
}

// FunctionCall is the inner function name + arguments of a ToolCall.
type FunctionCall struct {
	Name             string `json:"name"`
	Arguments        string `json:"arguments"` // raw JSON string of arguments
	ThoughtSignature string `json:"thought_signature,omitempty"`
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
