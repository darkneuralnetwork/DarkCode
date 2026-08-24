package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

func readCallLog(t *testing.T, path string) []CallLogEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open call log: %v", err)
	}
	defer f.Close()
	var entries []CallLogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e CallLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}

// TestCallLogDisabledByDefault: SetCallLogPath("") — the default — must not
// write anything, and must not error either (best-effort telemetry).
func TestCallLogDisabledByDefault(t *testing.T) {
	SetCallLogPath("")
	t.Cleanup(func() { SetCallLogPath("") })

	inner := &countingClient{err: errors.New("boom")}
	rc := WithRetry(inner, RetryOpts{MaxAttempts: 1})
	_, _ = rc.ChatCompletion(context.Background(), &core.CompletionRequest{Model: "test-model"})
	// No path was set — nothing to read, and recordCall must not have panicked
	// or blocked (the call above already proves that).
}

// TestCallLogRecordsEveryAttemptOfARetriedCall is the direct answer to "is it
// consuming way too many requests": a retried call must produce one log line
// per real attempt, sharing a CallID, so a reader can see exactly how many
// times the provider was actually hit for one logical call.
func TestCallLogRecordsEveryAttemptOfARetriedCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm_calls.jsonl")
	SetCallLogPath(path)
	t.Cleanup(func() { SetCallLogPath("") })

	timeoutErr := &url.Error{Op: "Post", URL: "https://api.example.com", Err: timeoutError{}}
	inner := &countingClient{err: timeoutErr}
	rc := WithRetry(inner, RetryOpts{MaxAttempts: 3})

	if _, err := rc.ChatCompletion(context.Background(), &core.CompletionRequest{Model: "test-model"}); err == nil {
		t.Fatal("expected an error")
	}

	entries := readCallLog(t, path)
	if len(entries) != 3 {
		t.Fatalf("expected 3 logged attempts (MaxAttempts=3, all failing), got %d", len(entries))
	}
	for i, e := range entries {
		if e.Method != "chat_completion" {
			t.Errorf("entry %d: method = %q, want chat_completion", i, e.Method)
		}
		if e.Model != "test-model" {
			t.Errorf("entry %d: model = %q, want test-model", i, e.Model)
		}
		if e.CallID != entries[0].CallID {
			t.Errorf("entry %d: call_id = %d, want %d (all attempts of one call must share it)", i, e.CallID, entries[0].CallID)
		}
		if e.Attempt != i+1 {
			t.Errorf("entry %d: attempt = %d, want %d", i, e.Attempt, i+1)
		}
		if e.Success {
			t.Errorf("entry %d: success = true, want false (every attempt failed)", i)
		}
		wantRetried := i < len(entries)-1
		if e.Retried != wantRetried {
			t.Errorf("entry %d: retried = %v, want %v", i, e.Retried, wantRetried)
		}
	}
}

// TestCallLogRecordsASuccessfulSingleAttempt: the common case — no retry
// needed — still produces exactly one entry, marked success and not retried.
func TestCallLogRecordsASuccessfulSingleAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm_calls.jsonl")
	SetCallLogPath(path)
	t.Cleanup(func() { SetCallLogPath("") })

	inner := &countingClient{}
	rc := WithRetry(inner, RetryOpts{MaxAttempts: 5})
	if _, err := rc.ChatCompletion(context.Background(), &core.CompletionRequest{Model: "test-model"}); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	entries := readCallLog(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 logged attempt, got %d", len(entries))
	}
	if !entries[0].Success || entries[0].Retried {
		t.Fatalf("expected success=true, retried=false, got %+v", entries[0])
	}
}

// TestCallLogTruncatesLongErrors: a provider's error body can be a large
// JSON blob; the persisted log must not grow unbounded per entry.
func TestCallLogTruncatesLongErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm_calls.jsonl")
	SetCallLogPath(path)
	t.Cleanup(func() { SetCallLogPath("") })

	inner := &countingClient{err: errors.New(strings.Repeat("x", 2000))}
	rc := WithRetry(inner, RetryOpts{MaxAttempts: 1})
	_, _ = rc.ChatCompletion(context.Background(), &core.CompletionRequest{Model: "test-model"})

	entries := readCallLog(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].Error) > 600 {
		t.Fatalf("error field not truncated: %d bytes", len(entries[0].Error))
	}
}
