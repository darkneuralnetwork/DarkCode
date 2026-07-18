package tools

import (
	"sync"
	"time"
)

// ============================================================================
// SELF-HEALING TOOL RUNTIME — per-tool circuit breaker.
//
// A tool that keeps failing (a flaky MCP server, a wedged terminal command, a
// down web endpoint) was previously retried identically forever — every agent
// turn could re-invoke it and re-fail, wasting turns/tokens and, in the loop,
// tripping stuck-detection instead of routing around the real problem. The
// breaker tracks consecutive execution failures per tool and, past a
// threshold, quarantines it for a growing backoff window. During quarantine
// the tool short-circuits with a clear "temporarily unavailable" message
// (which also steers the LLM to try another approach) instead of running. It
// auto-probes when the window expires and fully recovers on the first success.
//
// Only genuine execution failures count. Argument-validation failures,
// permission denials, and unknown-tool calls are the caller's/user's doing,
// not tool unreliability, and are recorded before this breaker is consulted —
// they never trip it.
// ============================================================================

const (
	breakerDefaultThreshold   = 3
	breakerDefaultBaseBackoff = 2 * time.Second
	breakerDefaultMaxBackoff  = 60 * time.Second
)

type breakerState struct {
	consecutiveFails int
	quarantinedUntil time.Time
	backoff          time.Duration // current window, grows on repeated tripping
}

// toolBreaker is a thread-safe per-tool circuit breaker. Keyed by tool name.
type toolBreaker struct {
	mu          sync.Mutex
	state       map[string]*breakerState
	threshold   int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	nowFn       func() time.Time // injectable for tests
}

func newToolBreaker() *toolBreaker {
	return &toolBreaker{
		state:       make(map[string]*breakerState),
		threshold:   breakerDefaultThreshold,
		baseBackoff: breakerDefaultBaseBackoff,
		maxBackoff:  breakerDefaultMaxBackoff,
		nowFn:       time.Now,
	}
}

// allow reports whether a call to the named tool may proceed. When the tool is
// quarantined it returns (false, remaining-quarantine); otherwise (true, 0).
// A call allowed after the quarantine window expires is the "half-open" probe:
// its recorded outcome decides recovery vs. re-quarantine.
func (b *toolBreaker) allow(name string) (bool, time.Duration) {
	if b == nil || b.threshold <= 0 {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state[name]
	if s == nil || s.quarantinedUntil.IsZero() {
		return true, 0
	}
	now := b.nowFn()
	if now.Before(s.quarantinedUntil) {
		return false, s.quarantinedUntil.Sub(now)
	}
	return true, 0
}

// recordSuccess clears all failure state for a tool (full recovery).
func (b *toolBreaker) recordSuccess(name string) {
	if b == nil || b.threshold <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.state[name]; ok {
		delete(b.state, name)
	}
}

// recordFailure registers an execution failure. On reaching the threshold the
// tool is quarantined for the current backoff, which then doubles (capped) so
// a tool that fails its post-quarantine probe stays out longer.
func (b *toolBreaker) recordFailure(name string) {
	if b == nil || b.threshold <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state[name]
	if s == nil {
		s = &breakerState{backoff: b.baseBackoff}
		b.state[name] = s
	}
	s.consecutiveFails++
	if s.consecutiveFails >= b.threshold {
		if s.backoff <= 0 {
			s.backoff = b.baseBackoff
		}
		s.quarantinedUntil = b.nowFn().Add(s.backoff)
		if next := s.backoff * 2; next <= b.maxBackoff {
			s.backoff = next
		} else {
			s.backoff = b.maxBackoff
		}
	}
}

// quarantined reports whether the named tool is currently quarantined (for
// diagnostics / status surfaces).
func (b *toolBreaker) quarantined(name string) bool {
	ok, _ := b.allow(name)
	return !ok
}
