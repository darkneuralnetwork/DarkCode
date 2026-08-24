package llm

// call_log.go — a durable, append-only record of every real attempt to reach
// an LLM provider (one line per attempt, not per logical call — a retried
// call produces multiple lines that share a CallID).
//
// This exists because reconstructing "how many real requests did we make,
// and why" after the fact used to require forensic work: cross-referencing
// the cognition cascade's log (which only records whether a QUERY escalated
// to the model, not how many provider round-trips that one query cost once
// tool-calling or retries were involved) against timestamps and guesswork.
// A free-tier daily quota question is the case that motivated this, but the
// same gap made every "why is this slow / why did this cost so much"
// question equally hard to answer from what was actually persisted.
//
// Mirrors orchestrator's cascade_log.jsonl: same JSONL-append shape, same
// best-effort "telemetry must never fail a request" contract, same
// disabled-until-a-path-is-set default (SetCallLogPath("") turns it off).

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/infra/core"
)

// CallLogEntry records one real attempt to reach a provider.
type CallLogEntry struct {
	Time        time.Time `json:"time"`
	CallID      uint64    `json:"call_id"`           // shared by every attempt of the same logical call
	Method      string    `json:"method"`            // "chat_completion" | "chat_completion_stream" | "create_embedding"
	Purpose     string    `json:"purpose,omitempty"` // "execute", "plan", "compress", ... — see modelport.Purpose
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Attempt     int       `json:"attempt"` // 1-indexed
	MaxAttempts int       `json:"max_attempts"`
	DurationMs  int64     `json:"duration_ms"`
	Success     bool      `json:"success"`
	Retried     bool      `json:"retried"` // true if this attempt will be followed by another
	StatusCode  int       `json:"status_code,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// purposeCtxKey is unexported so only this package's WithPurpose can set it.
type purposeCtxKey struct{}

// WithPurpose tags ctx with a short label for why a call is being made (the
// caller's own vocabulary — e.g. modelport.Purpose stringified: "execute",
// "plan", "compress", "classify"). This package doesn't know or care what
// the labels mean; it only carries and logs whatever the caller sets, so the
// call log can attribute cost to a subsystem, not just to a model.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	if purpose == "" {
		return ctx
	}
	return context.WithValue(ctx, purposeCtxKey{}, purpose)
}

func purposeFrom(ctx context.Context) string {
	p, _ := ctx.Value(purposeCtxKey{}).(string)
	return p
}

var (
	callLogMu   sync.Mutex
	callLogPath string
	nextCallID  uint64
)

// SetCallLogPath enables JSONL persistence of every real provider call
// attempt to path (one entry per attempt, appended). "" disables it. Package-
// level rather than per-client: every WithRetry-wrapped client in the
// process (one per registered model) shares one log, which is the point —
// a single file answers "what did we actually send, across every model,
// today."
func SetCallLogPath(path string) {
	callLogMu.Lock()
	callLogPath = path
	callLogMu.Unlock()
}

// newCallID hands out a process-lifetime-unique id so every attempt of one
// logical (possibly retried) call can be grouped by a reader.
func newCallID() uint64 {
	return atomic.AddUint64(&nextCallID, 1)
}

// recordCall appends one JSONL line, best-effort: a logging failure must
// never surface as a call failure.
func recordCall(e CallLogEntry) {
	callLogMu.Lock()
	path := callLogPath
	callLogMu.Unlock()
	if path == "" {
		return
	}
	e.Error = core.LogSafe(truncateForLog(e.Error, 500))
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data = append(data, '\n')
	_, _ = f.Write(data)
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
