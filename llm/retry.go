package llm

// retry.go — agent-level retry/backoff for LLM calls.
//
// This is the resilience layer the codebase was missing (audit: "router.callModel
// has zero retry/backoff/429 handling"). It mirrors pi's built-in `retry.enabled`
// ("automatic agent-level retry on transient errors") and is installed at every
// model-registration site (app_wireup.initRouter + kernel.ReloadModels) so ALL
// consumers — router consensus, sub-agents, the ReAct loop, executeDirectNoTools,
// and the context compressor — get retry transparently.
//
// Design:
//   - A typed *APIError (returned by llm.Client for non-200 responses) lets the
//     retry layer inspect the status code + Retry-After hint precisely.
//   - Retryable: HTTP 429, 5xx, and network errors. NOT other 4xx, NOT
//     decode/marshal failures (those are deterministic, not transient).
//   - Backoff: exponential with a ±25% jitter, capped at MaxDelay; honors the
//     server's Retry-After header (seconds or HTTP-date), capped at 60s.
//   - Stream-safe: a streaming call is retried ONLY if no content/tool-call
//     delta was delivered on the failed attempt — otherwise the UI would see
//     duplicated chunks. A 429 always happens before any SSE parsing (the
//     status check runs right after http.Client.Do), so pre-stream 429s are
//     always safe to retry; mid-stream errors are not retried.
//
// The wrapper is always on in every execution profile (Parallel, Sequential,
// Auto) — retry is resilience, not a mode feature.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// APIError is the typed error returned by Client.ChatCompletion and
// Client.ChatCompletionStream when the provider responds with a non-200 status.
// It carries the status code and the parsed Retry-After hint so the retry layer
// can decide whether to retry and how long to wait. Its Error() string is
// intentionally identical to the previous fmt.Errorf("API error %d: %s", …)
// format so any code that string-matches error messages keeps working.
type APIError struct {
	Code       int
	Body       string
	RetryAfter time.Duration // 0 if the header was absent or unparseable
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.Code, e.Body)
}

// parseRetryAfter reads the HTTP "Retry-After" response header and returns it as
// a Duration. The header may be either delta-seconds or an HTTP-date (RFC 7231).
// It also inspects the response body for Gemini-style rate limit hints.
// Returns 0 if absent or unparseable. Capped at retryAfterCap.
func parseRetryAfter(resp *http.Response, body string) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			if secs < 0 {
				return 0
			}
			d := time.Duration(secs) * time.Second
			if d > retryAfterCap {
				return retryAfterCap
			}
			return d
		}
		if t, err := http.ParseTime(v); err == nil {
			d := time.Until(t)
			if d < 0 {
				return 0
			}
			if d > retryAfterCap {
				return retryAfterCap
			}
			return d
		}
	}
	
	// Gemini specific body parsing: "Please retry in 1.5s" or "Please retry in 445.309714ms."
	if body != "" && strings.Contains(body, "Please retry in") {
		re := regexp.MustCompile(`Please retry in ([0-9.]+)(m?s)`)
		matches := re.FindStringSubmatch(body)
		if len(matches) == 3 {
			if secs, err := strconv.ParseFloat(matches[1], 64); err == nil && secs > 0 {
				var d time.Duration
				if matches[2] == "ms" {
					d = time.Duration(secs * float64(time.Millisecond))
				} else {
					d = time.Duration(secs * float64(time.Second))
				}
				if d > retryAfterCap {
					return retryAfterCap
				}
				return d
			}
		}
	}
	
	return 0
}

// retryAfterCap bounds how long we'll honor a server-supplied Retry-After.
const retryAfterCap = 90 * time.Second

// RetryOpts configures the retry layer.
type RetryOpts struct {
	MaxAttempts int           // total attempts including the first (>=1)
	BaseDelay   time.Duration // backoff for the first retry
	MaxDelay    time.Duration // backoff cap
}

// DefaultRetryOpts mirrors pi's "retry.enabled" politeness: bounded attempts,
// modest exponential backoff. Tuned to absorb a typical free-tier rate-limit
// burst (a few seconds) without stalling interactive chat.
var DefaultRetryOpts = RetryOpts{
	MaxAttempts: 5,
	BaseDelay:   1 * time.Second,
	MaxDelay:    60 * time.Second,
}

// RetryingClient wraps any core.LLMClient with automatic retry on transient
// errors. It is what gets registered in the router so every downstream caller
// benefits from retry without each one implementing it.
//
// inner is deliberately the core.LLMClient interface, not the concrete
// *Client type: wrapping *embedded.EmbeddedClient (which embeds *Client but
// overrides ChatCompletion/ChatCompletionStream/CreateEmbedding to add a
// model-swap guard) requires calling through the interface so those
// overrides are actually invoked. Previously WithRetry only accepted *Client,
// which forced every embedded-model call site to unwrap to the raw inner
// *llm.Client before wrapping — silently bypassing the swap guard for every
// router-registered local model.
type RetryingClient struct {
	inner core.LLMClient
	opts  RetryOpts
	mu    sync.Mutex
	rng   *rand.Rand
}

// WithRetry wraps any core.LLMClient with the default retry policy and
// returns a *RetryingClient (which itself implements core.LLMClient).
// Register the result in the router instead of the bare client.
func WithRetry(c core.LLMClient, opts RetryOpts) *RetryingClient {
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = DefaultRetryOpts.BaseDelay
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = DefaultRetryOpts.MaxDelay
	}
	return &RetryingClient{
		inner: c,
		opts:  opts,
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// providerIdentifier is satisfied by *Client (and, via struct embedding, by
// *embedded.EmbeddedClient) — a duck-typed accessor so Provider() below
// works without a concrete *Client type assertion.
type providerIdentifier interface {
	ProviderID() string
}

// Provider returns the wrapped client's provider id (used by the kernel's
// Auto-profile free-tier detector, which needs to know the provider even though
// the client is now wrapped).
func (c *RetryingClient) Provider() string {
	if c.inner == nil {
		return ""
	}
	if p, ok := c.inner.(providerIdentifier); ok {
		return p.ProviderID()
	}
	return ""
}

// Unwrap exposes the underlying client for type assertions / introspection.
func (c *RetryingClient) Unwrap() core.LLMClient { return c.inner }

// errNoInnerClient is returned when a RetryingClient has no underlying
// *Client (e.g. the compressor was wired before the local model loaded in a
// local-only setup). Returning a typed error — instead of panicking on a nil
// dereference — lets callers (the compressor) hit their existing error /
// fallback branch. This is the defense-in-depth net for the error.txt panic.
var errNoInnerClient = fmt.Errorf("llm: client not configured (no underlying model client)")

// ModelInfo delegates to the wrapped client (pure metadata, no network).
func (c *RetryingClient) ModelInfo() core.ModelMetadata {
	if c.inner == nil {
		return core.ModelMetadata{}
	}
	return c.inner.ModelInfo()
}

// Ping delegates to the wrapped client. A health check is not the parallel
// completion hot path that motivates retry, so it is not retried here — a
// failing ping should surface immediately rather than be masked by backoff.
func (c *RetryingClient) Ping(ctx context.Context) error {
	if c.inner == nil {
		return errNoInnerClient
	}
	return c.inner.Ping(ctx)
}

// Close delegates to the wrapped client.
func (c *RetryingClient) Close() error {
	if c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

// CreateEmbedding generates a vector embedding using the underlying client, with retries.
func (c *RetryingClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c.inner == nil {
		return nil, errNoInnerClient
	}
	var vec []float32
	var err error
	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		vec, err = c.inner.CreateEmbedding(ctx, text)
		if err == nil {
			return vec, nil
		}
		if !c.retryable(err) {
			return nil, err
		}
		if attempt < c.opts.MaxAttempts {
			if sleepErr := c.sleep(ctx, c.backoff(err, attempt)); sleepErr != nil {
				return nil, sleepErr
			}
		}
	}
	return vec, err
}

// ChatCompletion delegates to the wrapped client with retry on transient errors.
func (c *RetryingClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	if c.inner == nil {
		return nil, errNoInnerClient
	}
	var lastErr error
	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		resp, err := c.inner.ChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !c.retryable(err) {
			return nil, err
		}
		if attempt == c.opts.MaxAttempts {
			break
		}
		if err := c.sleep(ctx, c.backoff(err, attempt)); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.opts.MaxAttempts, lastErr)
}

// ChatCompletionStream delegates to the wrapped client with retry, but ONLY
// retries when no content/tool-call delta was delivered on the failed attempt —
// otherwise retrying would replay streamed chunks to the UI.
func (c *RetryingClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	if c.inner == nil {
		return nil, errNoInnerClient
	}
	var lastErr error
	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		streamed := false
		wrapped := cb
		if cb != nil {
			wrapped = &core.StreamCallbacks{
				OnContent: func(chunk string) {
					streamed = true
					if cb.OnContent != nil {
						cb.OnContent(chunk)
					}
				},
				OnToolCall: func(tc core.ToolCall) {
					streamed = true
					if cb.OnToolCall != nil {
						cb.OnToolCall(tc)
					}
				},
			}
		}
		resp, err := c.inner.ChatCompletionStream(ctx, req, wrapped)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// Never retry once streaming has started (partial output already sent).
		if streamed || !c.retryable(err) {
			return nil, err
		}
		if attempt == c.opts.MaxAttempts {
			break
		}
		if err := c.sleep(ctx, c.backoff(err, attempt)); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.opts.MaxAttempts, lastErr)
}

// retryable reports whether err represents a transient failure worth retrying:
// HTTP 429 / 5xx (via *APIError), or a network-level error. Deterministic
// failures (4xx other than 429, JSON decode/marshal errors) are not retried.
func (c *RetryingClient) retryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		// If the API explicitly says a quota limit is 0, retrying will never succeed.
		if strings.Contains(ae.Body, "limit: 0") {
			return false
		}
		return ae.Code == 429 || ae.Code >= 500
	}
	// Network errors: the client wraps connection failures as "send request: …".
	// Also catch any net.Error (timeouts, temporary errors) directly.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

// backoff computes the wait duration for the given attempt, honoring a
// server-supplied Retry-After when present and applying ±25% jitter.
func (c *RetryingClient) backoff(err error, attempt int) time.Duration {
	d := c.opts.BaseDelay * time.Duration(1<<(attempt-1)) // 1x,2x,4x,8x…
	if d <= 0 || d > c.opts.MaxDelay {
		d = c.opts.MaxDelay
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		if ae.RetryAfter > d {
			d = ae.RetryAfter
		}
	}
	c.mu.Lock()
	jitter := c.rng.Float64()*0.5 - 0.25 // -0.25 … +0.25
	c.mu.Unlock()
	d = time.Duration(float64(d) * (1 + jitter))
	if d < 0 {
		d = c.opts.BaseDelay
	}
	return d
}

// sleep waits for d (or until ctx is cancelled) and returns ctx.Err() on cancel.
func (c *RetryingClient) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
