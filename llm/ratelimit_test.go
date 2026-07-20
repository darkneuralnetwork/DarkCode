package llm

import (
	"context"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// stubClient is a minimal core.LLMClient whose ChatCompletion returns a
// scripted error (nil = success).
type stubClient struct {
	err   error
	calls int
}

func (s *stubClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Content: "ok"}}}}, nil
}
func (s *stubClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return s.ChatCompletion(ctx, req)
}
func (s *stubClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (s *stubClient) ModelInfo() core.ModelMetadata  { return core.ModelMetadata{} }
func (s *stubClient) Ping(ctx context.Context) error { return nil }
func (s *stubClient) Close() error                   { return nil }

// harness wires a RateLimitedClient to a fake clock: sleeps advance the
// clock instantly and are recorded.
func harness(inner core.LLMClient, limits RateLimits) (*RateLimitedClient, *[]time.Duration) {
	cur := time.Unix(1_700_000_000, 0)
	var sleeps []time.Duration
	rl := WithRateLimit(inner, limits)
	rl.nowFn = func() time.Time { return cur }
	rl.sleepFn = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		cur = cur.Add(d)
		return nil
	}
	return rl, &sleeps
}

func req() *core.CompletionRequest {
	return &core.CompletionRequest{Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}}}
}

func TestRateLimitUnlimitedNeverSleeps(t *testing.T) {
	rl, sleeps := harness(&stubClient{}, RateLimits{})
	for i := 0; i < 50; i++ {
		if _, err := rl.ChatCompletion(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sleeps) != 0 {
		t.Fatalf("unlimited client slept %v", *sleeps)
	}
}

func TestRateLimitRPMPacesAfterBurst(t *testing.T) {
	rl, sleeps := harness(&stubClient{}, RateLimits{RPM: 2})
	// Bucket starts full: 2 instant requests.
	for i := 0; i < 2; i++ {
		if _, err := rl.ChatCompletion(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sleeps) != 0 {
		t.Fatalf("burst within capacity slept %v", *sleeps)
	}
	// Third request must wait for a refill: 1 token at 2/min = 30s.
	if _, err := rl.ChatCompletion(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) == 0 {
		t.Fatal("third request over budget did not wait")
	}
	total := time.Duration(0)
	for _, d := range *sleeps {
		total += d
	}
	if total < 29*time.Second || total > 31*time.Second {
		t.Errorf("waited %v, want ~30s", total)
	}
}

func TestRateLimit429SetsHoldAndAdaptiveCap(t *testing.T) {
	inner := &stubClient{err: &APIError{Code: 429, Body: "rate limited", RetryAfter: 7 * time.Second}}
	rl, sleeps := harness(inner, RateLimits{})

	if _, err := rl.ChatCompletion(context.Background(), req()); err == nil {
		t.Fatal("expected the 429 to propagate")
	}
	rl.mu.Lock()
	adaptive := rl.adaptiveRPM
	rl.mu.Unlock()
	if adaptive == 0 {
		t.Error("429 did not set an adaptive RPM cap")
	}

	// Next call must honor the Retry-After hold.
	inner.err = nil
	if _, err := rl.ChatCompletion(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	held := time.Duration(0)
	for _, d := range *sleeps {
		held += d
	}
	if held < 7*time.Second {
		t.Errorf("held %v before next call, want >= 7s (Retry-After)", held)
	}
}

func TestRateLimitAdaptiveRestoresAndReleases(t *testing.T) {
	rl, _ := harness(&stubClient{}, RateLimits{})
	rl.mu.Lock()
	rl.adaptiveRPM = 100
	rl.last429 = rl.nowFn().Add(-10 * time.Minute) // long clean streak
	rl.mu.Unlock()

	// Successful calls grow the cap ~10% each until it releases entirely.
	for i := 0; i < 30; i++ {
		if _, err := rl.ChatCompletion(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
	}
	rl.mu.Lock()
	adaptive := rl.adaptiveRPM
	rl.mu.Unlock()
	if adaptive != 0 {
		t.Errorf("adaptive cap = %d after sustained success with no configured limit, want released (0)", adaptive)
	}
}

func TestLimitsForOnlyFlagsExplicitFreeModels(t *testing.T) {
	if l := LimitsFor("openrouter", "meta-llama/llama-3.3-70b:free"); l.RPM == 0 {
		t.Error(":free model should get a proactive RPM cap")
	}
	for _, m := range []string{"gpt-4o", "claude-sonnet-5", "deepseek-chat", "gemini-2.5-pro"} {
		if l := LimitsFor("whatever", m); l.RPM != 0 || l.TPM != 0 {
			t.Errorf("%s should be unlimited by default, got %+v", m, l)
		}
	}
}

func TestDailyQuotaIsNonRetryableWithCleanMessage(t *testing.T) {
	dailyBody := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]}]}}`
	ae := &APIError{Code: 429, Body: dailyBody}

	// Non-retryable: a per-day cap won't clear within a retry window.
	rc := WithRetry(&stubClient{err: ae}, DefaultRetryOpts)
	if rc.retryable(ae) {
		t.Error("daily-quota 429 must be non-retryable")
	}
	// Clean, actionable message — not the raw JSON blob.
	msg := ae.Error()
	if len(msg) > 200 || !contains(msg, "daily quota") {
		t.Errorf("daily-quota Error() not clean: %q", msg)
	}

	// A per-MINUTE quota is still retryable (recovers quickly).
	minute := &APIError{Code: 429, Body: `{"error":{"message":"quota","details":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier"}]}}`}
	if !rc.retryable(minute) {
		t.Error("per-minute 429 should remain retryable")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
