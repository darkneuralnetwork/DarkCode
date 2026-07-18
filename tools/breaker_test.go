package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
)

func TestBreakerTripsAfterThreshold(t *testing.T) {
	b := newToolBreaker() // threshold 3

	// Below threshold: still allowed.
	b.recordFailure("flaky")
	b.recordFailure("flaky")
	if ok, _ := b.allow("flaky"); !ok {
		t.Fatal("tool should still be allowed after 2 failures (threshold is 3)")
	}

	// Third consecutive failure trips the breaker.
	b.recordFailure("flaky")
	if ok, _ := b.allow("flaky"); ok {
		t.Fatal("tool should be quarantined after 3 consecutive failures")
	}
}

func TestBreakerSuccessResetsFailures(t *testing.T) {
	b := newToolBreaker()
	b.recordFailure("t")
	b.recordFailure("t")
	b.recordSuccess("t") // reset
	b.recordFailure("t")
	b.recordFailure("t")
	if ok, _ := b.allow("t"); !ok {
		t.Fatal("a success must reset the consecutive-failure count, so 2 more failures shouldn't trip it")
	}
}

func TestBreakerAutoRecoversAfterBackoff(t *testing.T) {
	b := newToolBreaker()
	now := time.Now()
	b.nowFn = func() time.Time { return now }

	b.recordFailure("t")
	b.recordFailure("t")
	b.recordFailure("t") // trips → quarantined for baseBackoff (2s)

	if ok, _ := b.allow("t"); ok {
		t.Fatal("should be quarantined immediately after tripping")
	}

	// Advance past the backoff window: the tool becomes probe-able again.
	now = now.Add(breakerDefaultBaseBackoff + time.Second)
	if ok, _ := b.allow("t"); !ok {
		t.Fatal("after the backoff window the tool should be allowed (half-open probe)")
	}

	// A successful probe fully recovers it.
	b.recordSuccess("t")
	if b.quarantined("t") {
		t.Fatal("a successful probe should fully recover the tool")
	}
}

func TestBreakerBackoffGrowsOnRepeatedTripping(t *testing.T) {
	b := newToolBreaker()
	now := time.Now()
	b.nowFn = func() time.Time { return now }

	trip := func() { b.recordFailure("t"); b.recordFailure("t"); b.recordFailure("t") }

	trip() // quarantine for base (2s)
	_, r1 := b.allow("t")

	// Expire, then fail the probe → re-quarantine for a longer window.
	now = now.Add(r1 + time.Second)
	b.recordFailure("t") // consecutiveFails already >= threshold → re-trips
	_, r2 := b.allow("t")

	if r2 <= r1 {
		t.Errorf("backoff should grow on repeated tripping: first ~%s, second ~%s", r1, r2)
	}
}

func TestBreakerDisabledWhenThresholdZero(t *testing.T) {
	b := newToolBreaker()
	b.threshold = 0 // disabled
	for i := 0; i < 10; i++ {
		b.recordFailure("t")
	}
	if ok, _ := b.allow("t"); !ok {
		t.Fatal("a disabled breaker (threshold 0) must never quarantine")
	}
}

// TestDispatchQuarantinesFlakyTool is the end-to-end check through the real
// dispatch path: a tool that fails 3× gets quarantined and the 4th dispatch
// short-circuits without running the handler.
func TestDispatchQuarantinesFlakyTool(t *testing.T) {
	r := NewRegistry()
	var runs int
	r.Register(&ToolEntry{
		Name:       "flaky",
		Parameters: MustParseSchema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			runs++
			return &ToolResult{Name: "flaky", Success: false, Error: "boom"}
		},
	})

	for i := 0; i < 3; i++ {
		r.DispatchAll(context.Background(), []core.ToolCall{call("x", "flaky", `{}`)})
	}
	if runs != 3 {
		t.Fatalf("handler ran %d times in the first 3 dispatches, want 3", runs)
	}

	// 4th dispatch: quarantined, handler must NOT run.
	res := r.DispatchAll(context.Background(), []core.ToolCall{call("x", "flaky", `{}`)}).([]DispatchResult)
	if runs != 3 {
		t.Errorf("handler ran during quarantine (%d times), want it short-circuited at 3", runs)
	}
	if res[0].Result.Success {
		t.Error("a quarantined dispatch should report failure")
	}
	if !strings.Contains(res[0].Result.Error, "quarantined") {
		t.Errorf("quarantine error = %q, want it to mention quarantine", res[0].Result.Error)
	}
}

// TestDispatchDoesNotQuarantineOnValidationFailure confirms non-execution
// failures (bad args) never trip the breaker — only genuine handler failures.
func TestDispatchDoesNotQuarantineOnValidationFailure(t *testing.T) {
	r := NewRegistry()
	var runs int
	r.Register(&ToolEntry{
		Name:       "needs_path",
		Parameters: MustParseSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			runs++
			return &ToolResult{Name: "needs_path", Success: true}
		},
	})

	// 5 calls that all fail validation (missing required arg) — the handler
	// never runs, so the breaker must never trip.
	for i := 0; i < 5; i++ {
		r.DispatchAll(context.Background(), []core.ToolCall{call("x", "needs_path", `{}`)})
	}
	if q := r.breaker.quarantined("needs_path"); q {
		t.Error("validation failures must not quarantine the tool")
	}
	// A valid call still works.
	res := r.DispatchAll(context.Background(), []core.ToolCall{call("x", "needs_path", `{"path":"/tmp/x"}`)}).([]DispatchResult)
	if !res[0].Result.Success {
		t.Error("a valid call should succeed after prior validation failures")
	}
}
