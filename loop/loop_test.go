package loop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
)

// fakeLLMClient is a scripted core.LLMClient — see orchestrator's identical
// helper for the rationale; duplicated here rather than shared across
// packages since Go doesn't allow importing another package's _test.go
// files, and this is small enough not to be worth a separate testutil
// package for two consumers.
type fakeLLMClient struct {
	name      string
	responses []string
	delay     time.Duration
	calls     int32
	// onCall, if set, is invoked at the start of every completion call. Used
	// by the cancellation test to cancel the context deterministically while
	// a call is in flight, instead of racing a wall-clock deadline.
	onCall func()
	// onRequest, if set, is invoked with each request before responding —
	// lets a test inspect exactly what messages were sent (e.g. to verify
	// prior conversation history was actually included).
	onRequest func(req *core.CompletionRequest)
	// failFn, if set, decides per-request whether the call fails. A single
	// error field wouldn't do: the interesting case is a client where the
	// THINK call works and only the auxiliary self-eval call fails, which is
	// exactly what an exhausted per-minute or per-day quota looks like from
	// inside a single run.
	failFn func(req *core.CompletionRequest) error
}

// isSelfEvalRequest identifies the one-line completion check by its distinctive
// shape, so failFn can target it without depending on call ordering.
func isSelfEvalRequest(req *core.CompletionRequest) bool {
	return req != nil && req.MaxTokens != nil && *req.MaxTokens == 60
}

func (f *fakeLLMClient) nextContent() string {
	idx := int(atomic.AddInt32(&f.calls, 1)) - 1
	if len(f.responses) == 0 {
		return "final answer"
	}
	if idx < len(f.responses) {
		return f.responses[idx]
	}
	return f.responses[len(f.responses)-1]
}

func (f *fakeLLMClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return f.ChatCompletionStream(ctx, req, nil)
}

func (f *fakeLLMClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	if f.onCall != nil {
		f.onCall()
	}
	if f.onRequest != nil {
		f.onRequest(req)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.failFn != nil {
		if err := f.failFn(req); err != nil {
			atomic.AddInt32(&f.calls, 1)
			return nil, err
		}
	}
	content := f.nextContent()
	if cb != nil && cb.OnContent != nil {
		cb.OnContent(content)
	}
	return &core.CompletionResponse{
		Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: content}}},
	}, nil
}

func (f *fakeLLMClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (f *fakeLLMClient) ModelInfo() core.ModelMetadata  { return core.ModelMetadata{ID: f.name} }
func (f *fakeLLMClient) Ping(ctx context.Context) error { return nil }
func (f *fakeLLMClient) Close() error                   { return nil }

func newTestRouter(client core.LLMClient) *router.Router {
	r := router.NewRouter(core.RouteSingle, nil)
	for _, tier := range []core.ModelTier{core.ModelTierCoding, core.ModelTierReasoning, core.ModelTierFast} {
		r.RegisterModel(tier, client, "fake-model")
	}
	r.MarkPrimary("fake-model")
	return r
}

func TestReActLoopSingleTurnNoTools(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"the final answer, no corrections needed."}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	result, err := l.Run(context.Background(), "answer a simple question", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output == "" {
		t.Error("expected a non-empty final answer")
	}
}

func TestReActLoopRespectsCancellationBeforeStarting(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"should never be reached"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := l.Run(ctx, "do something", nil)
	if err == nil {
		t.Fatal("expected Run to return an error for an already-cancelled context")
	}
}

// TestReActLoopCancellationMidRun verifies the loop propagates a context
// cancellation that happens while an LLM call is in flight, rather than
// running to completion or hanging. The cancellation is triggered
// deterministically from inside the fake's first call (not via a racy
// wall-clock deadline), so this test can't flake under different scheduling.
func TestReActLoopCancellationMidRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &fakeLLMClient{
		responses: []string{"first response", "second response", "third response"},
		delay:     time.Second, // long enough that ctx.Done() always wins after onCall cancels
		onCall:    cancel,      // cancel the context as soon as the first call begins
	}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 20)

	start := time.Now()
	_, err := l.Run(ctx, "do something slow", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a context-cancellation error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %s after cancellation, want it to return promptly", elapsed)
	}
}

func TestReActLoopMaxLoopsCeiling(t *testing.T) {
	// A tool schema-less registry means the model can only ever respond with
	// content, no tool calls — so with maxLoops=1 the very first no-tool-call
	// response should end the loop rather than needing more iterations.
	client := &fakeLLMClient{responses: []string{"done in one shot"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 1)

	result, err := l.Run(context.Background(), "quick task", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output == "" {
		t.Error("expected a non-empty answer within the loop ceiling")
	}
}

func TestNewClampsInvalidMaxLoops(t *testing.T) {
	client := &fakeLLMClient{}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 0)
	if l.maxLoops != DefaultMaxLoops {
		t.Errorf("maxLoops = %d, want DefaultMaxLoops (%d) when constructed with 0", l.maxLoops, DefaultMaxLoops)
	}
}

// TestReActLoopSelfEvalForcesAnotherIteration is the Fix A acceptance test:
// a self-evaluation that reports the goal isn't met must make the loop keep
// working (call the model again) instead of accepting the first no-tool-call
// response as final just because it was syntactically a stop condition.
// Sequence: [1] THINK -> "partial answer" (no tools, stop condition hit);
// [2] self-eval -> "GOAL_STATUS: CONTINUE: missing the edge case"; [3] THINK
// again -> "complete answer"; [4] self-eval -> "GOAL_STATUS: DONE".
func TestReActLoopSelfEvalForcesAnotherIteration(t *testing.T) {
	client := &fakeLLMClient{responses: []string{
		"partial answer",
		"GOAL_STATUS: CONTINUE: missing the edge case",
		"complete answer",
		"GOAL_STATUS: DONE",
	}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	result, err := l.Run(context.Background(), "handle all edge cases", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "complete answer" {
		t.Errorf("Output = %q, want %q (the loop should have kept working past the self-eval rejection)", result.Output, "complete answer")
	}
	if got := client.calls; got != 4 {
		t.Errorf("callCount = %d, want 4 (think, self-eval, think, self-eval)", got)
	}
}

// TestReActLoopSelfEvalRunsEvenAtTheIterationCeiling replaces a case that
// asserted the opposite. Self-eval used to be gated by
// `iteration < l.maxLoops` to save one call at the ceiling, which meant the
// final turn was the one turn that never had to justify stopping — with the
// shipped maxLoops of 3, that guard fired constantly and the loop's only
// semantic stop condition was skipped exactly when it mattered most.
//
// The check is cheap (one line, 60 tokens) and it is the difference between
// "the model stopped calling tools" and "the goal is met", so it now runs on
// every stop attempt. Corrections have their own budget, so it still cannot
// extend the work ceiling.
func TestReActLoopSelfEvalRunsEvenAtTheIterationCeiling(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"done in one shot", "GOAL_STATUS: DONE"}}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 1)

	result, err := l.Run(context.Background(), "quick task", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "done in one shot" {
		t.Errorf("Output = %q, want the single response unchanged", result.Output)
	}
	if !result.Completed {
		t.Error("Completed = false, want true — the self-eval confirmed the goal was met")
	}
	if got := client.calls; got != 2 {
		t.Errorf("callCount = %d, want 2 (think, self-eval)", got)
	}
}

// TestReActLoopUnrunnableSelfEvalDoesNotReportSuccess is the regression test
// for the earliest stop of all. evaluateGoalCompletion used to report "done"
// on any transport error, so on a metered free tier whose daily quota is
// measured in tens, an exhausted quota made every self-eval 429 — and every
// 429 read as "the goal is met". A multi-step task ended after its first
// answer and reported success.
func TestReActLoopUnrunnableSelfEvalDoesNotReportSuccess(t *testing.T) {
	// THINK succeeds; only the auxiliary self-eval call is rate-limited —
	// what an exhausted daily quota looks like mid-run.
	client := &fakeLLMClient{
		responses: []string{"a partial answer"},
		failFn: func(req *core.CompletionRequest) error {
			if isSelfEvalRequest(req) {
				return errors.New("429 rate limit exceeded")
			}
			return nil
		},
	}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 1)

	result, err := l.Run(context.Background(), "a long multi-step task", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Completed {
		t.Error("Completed = true, but the completion check never ran — an unverifiable answer must not be recorded as a success")
	}
}

// TestReActLoopHistoryIsIncluded is the Fix C acceptance test: prior
// conversation passed into Run must actually reach the model, giving the
// loop real continuity for a follow-up like "continue" instead of starting
// a fresh, memoryless conversation every time.
func TestReActLoopHistoryIsIncluded(t *testing.T) {
	var sawHistory bool
	client := &fakeLLMClient{
		responses: []string{"continuing the prior work", "GOAL_STATUS: DONE"},
		onRequest: func(req *core.CompletionRequest) {
			for _, m := range req.Messages {
				if m.ContentString() == "earlier turn: built the login form" {
					sawHistory = true
				}
			}
		},
	}
	l := New(newTestRouter(client), tools.NewRegistry(), nil, 5)

	history := []core.Message{
		{Role: core.RoleUser, Content: "build the login form"},
		{Role: core.RoleAssistant, Content: "earlier turn: built the login form"},
	}
	if _, err := l.Run(context.Background(), "continue", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawHistory {
		t.Error("expected prior conversation history to be included in the model's messages")
	}
}

func TestTruncateHistory_KeepsEverythingWithinBudget(t *testing.T) {
	history := []core.Message{
		{Role: core.RoleUser, Content: "short"},
		{Role: core.RoleAssistant, Content: "also short"},
	}
	got := truncateHistory(history, 1000)
	if len(got) != len(history) {
		t.Errorf("len(got) = %d, want %d (everything fits within the budget)", len(got), len(history))
	}
}

func TestTruncateHistory_KeepsMostRecentWhenOverBudget(t *testing.T) {
	history := []core.Message{
		{Role: core.RoleUser, Content: "oldest message, should be dropped"},
		{Role: core.RoleAssistant, Content: "middle message, should be dropped"},
		{Role: core.RoleUser, Content: "newest message, must survive"},
	}
	// Budget only large enough for the last message.
	got := truncateHistory(history, len("newest message, must survive")+5)
	if len(got) != 1 || got[0].Content != "newest message, must survive" {
		t.Errorf("truncateHistory kept %v, want only the newest message", got)
	}
}

func TestTruncateHistory_EmptyInputReturnsNil(t *testing.T) {
	if got := truncateHistory(nil, 1000); got != nil {
		t.Errorf("truncateHistory(nil, ...) = %v, want nil", got)
	}
}
