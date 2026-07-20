package llm

// ratelimit.go — proactive, adaptive per-model rate limiting.
//
// The retry layer (retry.go) is REACTIVE: it absorbs a 429 after the
// provider already rejected the call. This layer is PROACTIVE: a token
// bucket paces requests (and estimated tokens) per minute so parallel DAG
// waves stop tripping free-tier limits in the first place, and ADAPTIVE: a
// real 429 halves the learned request rate and honors Retry-After as a hard
// hold, then the rate gently restores on sustained success — so paid tiers
// pay zero overhead and unconfigured free tiers converge to a safe pace
// after a single 429 instead of burning the retry budget every wave.
//
// Wrap order is WithRetry(WithRateLimit(client)): each retry attempt
// re-enters the limiter, so backed-off retries also respect the learned
// pace and the hold window.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// RateLimits configures the proactive budget for one model client.
// Zero values mean "no configured limit" — the adaptive learner still
// engages on observed 429s.
type RateLimits struct {
	RPM int // requests per minute
	TPM int // estimated tokens per minute
}

// LimitsFor returns conservative proactive defaults ONLY for models that are
// unambiguously free-tier (an explicit ":free" variant). Everything else is
// left unlimited so paid tiers are never throttled by a guess — the adaptive
// 429 learner protects them instead.
func LimitsFor(provider, model string) RateLimits {
	if strings.Contains(strings.ToLower(model), ":free") {
		return RateLimits{RPM: 18} // OpenRouter free models allow ~20/min
	}
	return RateLimits{}
}

// WrapCloud is the standard decoration for a cloud model client:
// retry(ratelimit(client)). Local/embedded clients should NOT use this —
// they have no provider-side rate limits.
func WrapCloud(c core.LLMClient, provider, model string) core.LLMClient {
	return WithRetry(WithRateLimit(c, LimitsFor(provider, model)), DefaultRetryOpts)
}

// tokenBucket is a standard refill bucket. grant deducts and returns 0 when
// n tokens are available now, otherwise returns how long to wait (without
// deducting).
type tokenBucket struct {
	perMin float64
	tokens float64
	last   time.Time
}

func newBucket(perMin float64, now time.Time) *tokenBucket {
	// Start full so short bursts (a first DAG wave) pass immediately.
	return &tokenBucket{perMin: perMin, tokens: perMin, last: now}
}

func (b *tokenBucket) grant(now time.Time, n float64) time.Duration {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.perMin / 60
		if b.tokens > b.perMin {
			b.tokens = b.perMin
		}
	}
	b.last = now
	if b.tokens >= n {
		b.tokens -= n
		return 0
	}
	need := n - b.tokens
	return time.Duration(need / (b.perMin / 60) * float64(time.Second))
}

// adaptiveReleaseRPM: once the learned rate has been restored past this and
// there is no configured cap, the constraint is dropped entirely (the 429
// was evidently transient, not a standing tier limit).
const adaptiveReleaseRPM = 240

// RateLimitedClient wraps any core.LLMClient with the proactive+adaptive
// budget. It implements core.LLMClient.
type RateLimitedClient struct {
	inner core.LLMClient
	cfg   RateLimits

	mu          sync.Mutex
	reqBucket   *tokenBucket
	tokBucket   *tokenBucket
	adaptiveRPM int       // learned cap from observed 429s; 0 = none
	holdUntil   time.Time // Retry-After hard hold
	last429     time.Time
	recent      []time.Time // request timestamps in the last minute

	// injectable for tests
	nowFn   func() time.Time
	sleepFn func(ctx context.Context, d time.Duration) error
}

// WithRateLimit wraps a client with the given proactive limits (zero =
// adaptive-only).
func WithRateLimit(c core.LLMClient, limits RateLimits) *RateLimitedClient {
	rl := &RateLimitedClient{
		inner: c,
		cfg:   limits,
		nowFn: time.Now,
	}
	rl.sleepFn = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	return rl
}

// effectiveRPMLocked resolves the current request cap: the tighter of the
// configured and learned limits; 0 = unlimited.
func (c *RateLimitedClient) effectiveRPMLocked() int {
	rpm := c.cfg.RPM
	if c.adaptiveRPM > 0 && (rpm == 0 || c.adaptiveRPM < rpm) {
		rpm = c.adaptiveRPM
	}
	return rpm
}

// acquire blocks (context-aware) until the request fits the budget.
func (c *RateLimitedClient) acquire(ctx context.Context, estTokens float64) error {
	for {
		c.mu.Lock()
		now := c.nowFn()
		var wait time.Duration

		if now.Before(c.holdUntil) {
			wait = c.holdUntil.Sub(now)
		} else {
			if rpm := c.effectiveRPMLocked(); rpm > 0 {
				if c.reqBucket == nil {
					c.reqBucket = newBucket(float64(rpm), now)
				}
				c.reqBucket.perMin = float64(rpm)
				wait = c.reqBucket.grant(now, 1)
			}
			if wait == 0 && c.cfg.TPM > 0 {
				if c.tokBucket == nil {
					c.tokBucket = newBucket(float64(c.cfg.TPM), now)
				}
				wait = c.tokBucket.grant(now, estTokens)
			}
		}

		if wait == 0 {
			// Record the dispatch for the 429 rate estimator.
			cutoff := now.Add(-time.Minute)
			kept := c.recent[:0]
			for _, t := range c.recent {
				if t.After(cutoff) {
					kept = append(kept, t)
				}
			}
			c.recent = append(kept, now)
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		if err := c.sleepFn(ctx, wait); err != nil {
			return err
		}
	}
}

// observe updates the adaptive state from a call's outcome.
func (c *RateLimitedClient) observe(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowFn()

	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 429 {
		c.last429 = now
		hold := ae.RetryAfter
		if hold <= 0 {
			hold = 5 * time.Second
		}
		if until := now.Add(hold); until.After(c.holdUntil) {
			c.holdUntil = until
		}
		// Learn: cap at half the observed request rate over the last minute.
		observed := len(c.recent)
		if observed < 2 {
			observed = 2
		}
		newRPM := observed / 2
		if newRPM < 1 {
			newRPM = 1
		}
		if c.adaptiveRPM == 0 || newRPM < c.adaptiveRPM {
			c.adaptiveRPM = newRPM
		}
		return
	}

	// Gentle restore on success: after two clean minutes, grow ~10%/call;
	// once clearly past any plausible free-tier ceiling (and nothing is
	// configured), release the constraint entirely.
	if err == nil && c.adaptiveRPM > 0 && now.Sub(c.last429) > 2*time.Minute {
		step := c.adaptiveRPM / 10
		if step < 1 {
			step = 1
		}
		c.adaptiveRPM += step
		if c.cfg.RPM == 0 && c.adaptiveRPM >= adaptiveReleaseRPM {
			c.adaptiveRPM = 0
		}
	}
}

// estimateRequestTokens approximates a request's token cost for the TPM
// bucket: the prompt (via the shared rune-aware estimator in client.go) plus
// the completion budget.
func estimateRequestTokens(req *core.CompletionRequest) float64 {
	prompt := 0
	for _, m := range req.Messages {
		prompt += estimateTokens(m.ContentString())
	}
	out := 800
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out = *req.MaxTokens
	}
	return float64(prompt + out)
}

// --- core.LLMClient implementation ---------------------------------------

func (c *RateLimitedClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	if err := c.acquire(ctx, estimateRequestTokens(req)); err != nil {
		return nil, err
	}
	resp, err := c.inner.ChatCompletion(ctx, req)
	c.observe(err)
	return resp, err
}

func (c *RateLimitedClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	if err := c.acquire(ctx, estimateRequestTokens(req)); err != nil {
		return nil, err
	}
	resp, err := c.inner.ChatCompletionStream(ctx, req, cb)
	c.observe(err)
	return resp, err
}

func (c *RateLimitedClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	if err := c.acquire(ctx, float64(len(text))/4); err != nil {
		return nil, err
	}
	out, err := c.inner.CreateEmbedding(ctx, text)
	c.observe(err)
	return out, err
}

func (c *RateLimitedClient) ModelInfo() core.ModelMetadata { return c.inner.ModelInfo() }
func (c *RateLimitedClient) Ping(ctx context.Context) error { return c.inner.Ping(ctx) }
func (c *RateLimitedClient) Close() error                  { return c.inner.Close() }

// ProviderID forwards the duck-typed provider accessor so RetryingClient
// (and the kernel's free-tier detector) still see the provider through this
// wrapper.
func (c *RateLimitedClient) ProviderID() string {
	if p, ok := c.inner.(providerIdentifier); ok {
		return p.ProviderID()
	}
	return ""
}

// Unwrap exposes the underlying client for introspection.
func (c *RateLimitedClient) Unwrap() core.LLMClient { return c.inner }
