package core

import "encoding/json"

// CompletionRequest is the request body sent to the LLM API.
type CompletionRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Tools         []ToolSchema   `json:"tools,omitempty"`
	ToolChoice    interface{}    `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions controls streaming behaviour.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ToolSchema is the OpenAI function-calling tool definition.
type ToolSchema struct {
	Type     string      `json:"type"` // always "function"
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema as raw bytes
}

// ChatChoice represents one choice in the completion response.
type ChatChoice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// CompletionResponse is the full non-streaming response from the LLM API.
type CompletionResponse struct {
	ID      string        `json:"id"`
	Choices []ChatChoice  `json:"choices"`
	Usage   ResponseUsage `json:"usage"`
}

type ResponseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails carries the provider's breakdown of the prompt
	// tokens — notably how many were served from the prefix cache. OpenAI and
	// the Anthropic/Gemini OpenAI-compatible endpoints all report cached
	// tokens here. Absent (nil) when the provider doesn't report caching.
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails is the provider's prompt-token breakdown; CachedTokens
// are the (much cheaper) tokens read from the prefix cache instead of being
// re-processed. Making them visible is what lets the cost meter and governor
// stop charging full input price for a cached prefix.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// CachedPromptTokens returns the cached prompt-token count (0 when the
// provider didn't report caching), so callers don't have to nil-check.
func (u ResponseUsage) CachedPromptTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// StreamEvent represents a single SSE event from the streaming API.
type StreamEvent struct {
	Choices []StreamChoice `json:"choices"`
	Usage   *ResponseUsage `json:"usage,omitempty"`
}

type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []StreamToolCall `json:"tool_calls,omitempty"`
}

// StreamToolCall is a tool call delta in a streaming response.
type StreamToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function,omitempty"`
}

// StreamCallbacks holds optional callbacks for streaming events.
type StreamCallbacks struct {
	OnContent  func(text string)   // called for each content delta
	OnToolCall func(call ToolCall) // called when a tool call is detected
}

// ModelMetadata represents basic info about a model's capabilities.
type ModelMetadata struct {
	ID        string
	Context   int
	SizeBytes int64
}

// EmbeddingRequest represents a request to the embeddings API.
type EmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbeddingResponse represents a response from the embeddings API.
type EmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage ResponseUsage `json:"usage"`
}
