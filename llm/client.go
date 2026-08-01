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
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/metrics"
	"github.com/darkcode/safeurl"
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

	// Keys, when set, rotates across several credentials and parks any that
	// get throttled. nil means "use APIKey", which is the single-key default.
	Keys *KeyPool

	// Effort is the default reasoning effort ("low"/"medium"/"high") sent when
	// a request does not set its own. Empty omits the field entirely.
	Effort string
}

// pickKey returns the credential to use for one request: the next healthy key
// from the pool, or the single configured key.
func (c *Client) pickKey() string {
	if k := c.Keys.Get(); k != "" {
		return k
	}
	return c.APIKey
}

// keyCooldown is how long a throttled credential is parked. Long enough to
// clear a per-minute quota, short enough that a small pool keeps working.
const keyCooldown = 60 * time.Second

// penalize parks the credential a failed request used, when the failure is one
// that another key could avoid (throttling or a rejected key).
func (c *Client) penalize(key string, err error) {
	var ae *APIError
	if !errors.As(err, &ae) || (ae.Code != 429 && ae.Code != 401 && ae.Code != 403) {
		return
	}
	d := keyCooldown
	if ae.RetryAfter > 0 {
		d = ae.RetryAfter
	}
	c.Keys.Penalize(key, d)
}

// NewClient creates a new LLM client.
func NewClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		// EgressClient applies no SSRF restrictions (a provider may legitimately
		// be a private vLLM host) but does enforce air-gap mode, so enabling it
		// stops cloud calls at the socket rather than trusting configuration.
		HTTPClient: safeurl.EgressClient(300 * time.Second),
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

// setAuth applies the provider-specific auth headers to an HTTP request using
// the given credential (from pickKey, so a pooled key rotates per request).
func (c *Client) setAuth(req *http.Request, key string) {
	req.Header.Set("Content-Type", "application/json")

	if c.Provider == "anthropic" && key != "" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		for k, v := range c.ExtraHeaders {
			req.Header.Set(k, v)
		}
		return
	}

	switch c.AuthScheme {
	case config.AuthAPIKey:
		req.Header.Set("api-key", key)
	case config.AuthNone:
		// no auth header
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}
	// Google Gemini API gateway often rejects Bearer for API keys.
	if c.Provider == "google" && key != "" {
		req.Header.Set("x-goog-api-key", key)
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
		prompt = core.EstimateTokens(messagesText(req.Messages))
	}
	if completion == 0 && resp != nil && len(resp.Choices) > 0 {
		completion = core.EstimateTokens(resp.Choices[0].Message.Content)
	}
	if total == 0 {
		total = prompt + completion
	}

	metrics.Default.Record(metrics.RequestRecord{
		ID:               randomID(),
		Timestamp:        time.Now(),
		Model:            strutil.NonEmpty(req.Model, c.Model),
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
		if s, ok := msgs[i].Content.(string); ok && strings.TrimSpace(s) == "" {
			msgs[i].Content = nil
		}
		for j := range msgs[i].ToolCalls {
			if strings.TrimSpace(msgs[i].ToolCalls[j].Function.Arguments) == "" {
				msgs[i].ToolCalls[j].Function.Arguments = "{}"
			}
		}
	}
}

// ChatCompletion sends a non-streaming chat completion request.
func (c *Client) ChatCompletion(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if err := c.checkEndpoint(); err != nil {
		return nil, err
	}
	req.Stream = false
	// Default the model to this client's configured model when the caller
	// left it blank. The request body marshals req.Model directly, so an
	// empty value sends `"model":""` — which providers like Gemini reject
	// with "model is not specified". This is the single safety net for every
	// call site that builds a request without a Model (the "blank name"
	// error class); the metrics path already used the same fallback.
	if req.Model == "" {
		req.Model = c.Model
	}
	sanitizeMessages(req.Messages)
	body, err := json.Marshal(c.outgoing(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	key := c.pickKey()
	c.setAuth(httpReq, key)

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
		apiErr := &APIError{Code: resp.StatusCode, Body: string(raw), RetryAfter: parseRetryAfter(resp, string(raw))}
		c.penalize(key, apiErr)
		return nil, apiErr
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
	// See ChatCompletion: default a blank model to this client's own so the
	// body never sends `"model":""` (the Gemini blank-name rejection).
	if req.Model == "" {
		req.Model = c.Model
	}
	sanitizeMessages(req.Messages)
	body, err := json.Marshal(c.outgoing(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	key := c.pickKey()
	c.setAuth(httpReq, key)
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
		apiErr := &APIError{Code: resp.StatusCode, Body: string(raw), RetryAfter: parseRetryAfter(resp, string(raw))}
		c.penalize(key, apiErr)
		return nil, apiErr
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
	// 0 means "not recognised", and the caller falls back to the configured
	// context length. That is deliberately different from the guess this used
	// to make: 8,000 for everything, which every current cloud model got and
	// which is out by a factor of 131 on Gemini 2.5 Flash. Compression then
	// fired long before the window was full, spending a call to discard
	// context that would have fit. See window.go.
	return core.ModelMetadata{
		ID:      c.Model,
		Context: ContextWindowFor(c.Model),
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
	c.setAuth(req, c.pickKey())

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
