package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/metrics"
)

// Type aliases so the rest of the llm package reads naturally.
type Message = core.Message
type ToolCall = core.ToolCall
type FunctionCall = core.FunctionCall
type CompletionRequest = core.CompletionRequest
type StreamOptions = core.StreamOptions
type ToolSchema = core.ToolSchema
type FunctionDef = core.FunctionDef
type ChatChoice = core.ChatChoice
type ResponseMessage = core.ResponseMessage
type CompletionResponse = core.CompletionResponse
type ResponseUsage = core.ResponseUsage
type StreamEvent = core.StreamEvent
type StreamChoice = core.StreamChoice
type Delta = core.Delta
type StreamToolCall = core.StreamToolCall
type StreamCallbacks = core.StreamCallbacks

// Client is an OpenAI-compatible LLM client with streaming support.
type Client struct {
	BaseURL      string
	APIKey       string
	HTTPClient   *http.Client
	Model        string
	Provider     string            // provider id from the registry (for metrics + auth)
	AuthScheme   string            // "bearer" (default), "api-key", "none"
	ExtraHeaders map[string]string // additional headers per provider
	ExtraQuery   string            // e.g. "api-version=..." appended to request URL
}

// NewClient creates a new LLM client.
func NewClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 300 * time.Second},
		Model:      model,
		AuthScheme: config.AuthBearer,
	}
}

// ProviderID returns the provider id this client is associated with. Lets
// callers holding only a core.LLMClient interface (e.g. RetryingClient) get
// the provider without a concrete *Client type assertion.
func (c *Client) ProviderID() string {
	return c.Provider
}

// SetProvider associates this client with a provider id from the registry.
// This resolves the correct auth scheme and any extra headers/query params.
func (c *Client) SetProvider(providerID string) *Client {
	c.Provider = providerID
	if p, ok := config.LookupProvider(providerID); ok {
		c.AuthScheme = p.AuthScheme
		if len(p.ExtraHeaders) > 0 {
			c.ExtraHeaders = make(map[string]string, len(p.ExtraHeaders))
			for k, v := range p.ExtraHeaders {
				c.ExtraHeaders[k] = v
			}
		}
		if p.ExtraQuery != "" {
			c.ExtraQuery = p.ExtraQuery
		}
	}
	return c
}

// SetAuthScheme overrides the authentication scheme.
func (c *Client) SetAuthScheme(scheme string) *Client {
	c.AuthScheme = scheme
	return c
}



// setAuth applies the provider-specific auth headers to an HTTP request.
func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")

	// Anthropic authenticates exclusively with `x-api-key` + `anthropic-version`.
	// It must NOT also receive `Authorization: Bearer` — the dual header breaks
	// auth (root cause of the Anthropic model-integration failures). Handle it
	// before the generic switch so Bearer is never emitted for Anthropic.
	if c.Provider == "anthropic" && c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		for k, v := range c.ExtraHeaders {
			req.Header.Set(k, v)
		}
		return
	}

	switch c.AuthScheme {
	case config.AuthAPIKey:
		req.Header.Set("api-key", c.APIKey)
	case config.AuthNone:
		// no auth header
	default:
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}
	// Google Gemini API gateway often rejects Bearer for API keys.
	if c.Provider == "google" && c.APIKey != "" {
		req.Header.Set("x-goog-api-key", c.APIKey)
	}
}

// ErrNoBaseURL is returned before any HTTP attempt when a client has no base
// URL configured. It is deliberately a plain (non-net.Error) error so the
// retry layer classifies it as permanent — retrying a request with no URL
// only produces the same `unsupported protocol scheme ""` failure five times.
// A client with an empty BaseURL should never be registered (see
// app_wireup.endpointUsable); this is the defense-in-depth at the call site.
var ErrNoBaseURL = errors.New("no base URL configured for this model (set a provider/base_url, or enable a local model)")

// checkEndpoint validates the client can actually build a request URL. Called
// at the top of every request method so an unusable client fails fast with a
// clear, non-retryable error instead of an opaque URL-scheme error deep in
// net/http.
func (c *Client) checkEndpoint() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return ErrNoBaseURL
	}
	return nil
}

// endpointURL builds the full request URL, appending any extra query params.
func (c *Client) endpointURL(path string) string {
	url := c.BaseURL + path
	if c.ExtraQuery != "" {
		url += "?" + c.ExtraQuery
	}
	return url
}

// recordUsage reports a completed (or failed) request to the metrics tracker.
// When the API did not return usage (some streaming providers omit it),
// tokens are estimated from the request/response sizes.
func (c *Client) recordUsage(req *CompletionRequest, resp *CompletionResponse, latency time.Duration, success bool) {
	var prompt, completion, total, cached int
	if resp != nil {
		prompt = resp.Usage.PromptTokens
		completion = resp.Usage.CompletionTokens
		total = resp.Usage.TotalTokens
		cached = resp.Usage.CachedPromptTokens()
	}
	if total == 0 && (prompt != 0 || completion != 0) {
		total = prompt + completion
	}
	// Estimate when the provider did not report usage.
	if prompt == 0 && len(req.Messages) > 0 {
		prompt = estimateTokens(messagesText(req.Messages))
	}
	if completion == 0 && resp != nil && len(resp.Choices) > 0 {
		completion = estimateTokens(resp.Choices[0].Message.Content)
	}
	if total == 0 {
		total = prompt + completion
	}

	metrics.Default.Record(metrics.RequestRecord{
		ID:               randomID(),
		Timestamp:        time.Now(),
		Model:            nonEmpty(req.Model, c.Model),
		Provider:         c.Provider,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CachedTokens:     cached,
		TotalTokens:      total,
		LatencyMs:        latency.Milliseconds(),
		Stream:           req.Stream,
		Success:          success,
	})
}

// estimateTokens gives a rough token count for costless telemetry when a
// provider does not return usage data. It is rune-aware so that CJK text
// (which tokenizes at roughly 1.5 chars/token for q4/BPE models) is not
// over-counted the way a naive byte-length/4 heuristic would. ASCII keeps the
// classic ~4 chars/token estimate so the common case is unchanged.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var asciiChars, cjkChars, otherChars int
	for _, r := range s {
		switch {
		case r < 0x80:
			asciiChars++
		case r >= 0x3000 && r <= 0x30FF, // CJK symbols + Japanese kana
			r >= 0x3400 && r <= 0x4DBF, // CJK Extension A
			r >= 0x4E00 && r <= 0x9FFF, // CJK Unified Ideographs
			r >= 0xAC00 && r <= 0xD7AF, // Hangul Syllables
			r >= 0xF900 && r <= 0xFAFF, // CJK Compatibility Ideographs
			r >= 0xFF00 && r <= 0xFFEF: // Halfwidth/Fullwidth Forms
			cjkChars++
		default:
			otherChars++
		}
	}
	tokens := asciiChars/4 + cjkChars*2/3 + otherChars/2
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// messagesText flattens a message slice into a single string for estimation.
func messagesText(msgs []Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.ContentString())
		sb.WriteByte(' ')
		for _, tc := range m.ToolCalls {
			sb.WriteString(tc.Function.Name)
			sb.WriteString(tc.Function.Arguments)
		}
	}
	return sb.String()
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func randomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b)
}

// sanitizeMessages ensures that any Message with an empty string content
// has its Content set to nil so it is properly omitted in JSON, avoiding
// "contents is not specified" errors on Google's Gemini API.
func sanitizeMessages(msgs []core.Message) {
	for i := range msgs {
		if s, ok := msgs[i].Content.(string); ok && s == "" {
			msgs[i].Content = nil
		}
	}
}

// ChatCompletion sends a non-streaming chat completion request.
func (c *Client) ChatCompletion(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if err := c.checkEndpoint(); err != nil {
		return nil, err
	}
	req.Stream = false
	sanitizeMessages(req.Messages)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuth(httpReq)

	start := time.Now()
	resp, err := c.HTTPClient.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		c.recordUsage(req, nil, latency, false)
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		c.recordUsage(req, nil, latency, false)
		return nil, &APIError{Code: resp.StatusCode, Body: string(raw), RetryAfter: parseRetryAfter(resp, string(raw))}
	}

	var result CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.recordUsage(req, nil, latency, false)
		return nil, fmt.Errorf("decode response: %w", err)
	}
	c.recordUsage(req, &result, latency, true)
	return &result, nil
}

// CreateEmbedding generates an embedding for the given text.
func (c *Client) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	if err := c.checkEndpoint(); err != nil {
		return nil, err
	}
	reqBody := map[string]interface{}{
		"model": c.Model,
		"input": text,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/embeddings"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Provider == "anthropic" {
		httpReq.Header.Set("x-api-key", c.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do embedding request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("empty embedding returned")
	}
	return result.Data[0].Embedding, nil
}

// ChatCompletionStream sends a streaming chat completion request,
// invoking callbacks for each content/tool-call delta. Returns the
// fully assembled response when done.
func (c *Client) ChatCompletionStream(ctx context.Context, req *CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error) {
	if err := c.checkEndpoint(); err != nil {
		return nil, err
	}
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true} // request usage in final chunk
	sanitizeMessages(req.Messages)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuth(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	start := time.Now()
	resp, err := c.HTTPClient.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		c.recordUsage(req, nil, latency, false)
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		c.recordUsage(req, nil, latency, false)
		return nil, &APIError{Code: resp.StatusCode, Body: string(raw), RetryAfter: parseRetryAfter(resp, string(raw))}
	}

	// Parse SSE stream
	var contentBuilder strings.Builder
	toolCallMap := make(map[int]*ToolCall)
	var finishReason string
	var streamUsage *ResponseUsage

	scanner := NewSSEScanner(resp.Body)
	for scanner.Scan() {
		data := scanner.Text()
		if data == "[DONE]" {
			break
		}
		if !strings.HasPrefix(data, "{") {
			continue
		}

		var event StreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		// Some providers send usage in a final chunk with no choices.
		if event.Usage != nil {
			streamUsage = event.Usage
		}

		for _, choice := range event.Choices {
			// Content delta
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
				if cb != nil && cb.OnContent != nil {
					cb.OnContent(choice.Delta.Content)
				}
			}

			// Tool call deltas (accumulated by index)
			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				existing, ok := toolCallMap[idx]
				if !ok {
					existing = &ToolCall{
						Type: "function",
					}
					toolCallMap[idx] = existing
				}
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				if tc.Function.ThoughtSignature != "" {
					existing.Function.ThoughtSignature += tc.Function.ThoughtSignature
				}
				existing.Function.Arguments += tc.Function.Arguments
			}

			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		c.recordUsage(req, nil, latency, false)
		return nil, fmt.Errorf("read stream: %w", err)
	}

	// Build final response
	var toolCalls []ToolCall
	for _, tc := range toolCallMap {
		if tc.Function.Name != "" {
			if cb != nil && cb.OnToolCall != nil {
				cb.OnToolCall(*tc)
			}
			toolCalls = append(toolCalls, *tc)
		}
	}

	content := contentBuilder.String()
	msg := ResponseMessage{
		Role:      "assistant",
		Content:   content,
		ToolCalls: toolCalls,
	}

	finalResp := &CompletionResponse{
		Choices: []ChatChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
	}
	if streamUsage != nil {
		finalResp.Usage = *streamUsage
	}
	c.recordUsage(req, finalResp, latency, true)
	return finalResp, nil
}

// ModelInfo returns metadata about this client's model.
func (c *Client) ModelInfo() core.ModelMetadata {
	// A simple heuristic for now. Proper provider logic will override this.
	ctxLen := 8000
	if strings.Contains(strings.ToLower(c.Model), "32k") || strings.Contains(strings.ToLower(c.Model), "claude-3") {
		ctxLen = 32000
	}
	return core.ModelMetadata{
		ID:      c.Model,
		Context: ctxLen,
	}
}

// Ping checks if the provider is reachable. It issues a lightweight GET to
// the OpenAI-compatible /models endpoint with a short timeout and treats any
// 2xx response as healthy. This is the honest replacement for the previous
// `return nil` stub, which reported every provider (including ones with an
// empty BaseURL) as healthy.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.checkEndpoint(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpointURL("/models"), nil)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	c.setAuth(req)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("ping: unexpected status %d", resp.StatusCode)
}

// Close cleans up resources.
func (c *Client) Close() error {
	return nil
}
