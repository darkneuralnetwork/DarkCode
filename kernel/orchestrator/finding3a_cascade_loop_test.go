package orchestrator

// finding3a_cascade_loop_test.go — QA audit Finding 3a: an explicit /loop
// request was silently answered by the cognition cascade instead of ever
// reaching the agentic loop. This proves the cascade is skipped once
// loopEnabledForRequest is true, using the same cache-seeding pattern
// cascade_test.go's rung-1 test uses to prove the opposite: without a loop
// override, the same goal answers from the cache with zero LLM calls.

import (
	"context"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
)

func TestCascadeAnsweredWithoutLoopOverride(t *testing.T) {
	deps := newTestKernel(t, nil)
	goal := "summarize our approach about memory retrieval tests"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: goal, Outcome: "success", Summary: goal, Output: "cached-answer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "cached-answer" {
		t.Fatalf("expected the cascade to answer from cache, got %q", out)
	}
	if deps.Client.callCount() != 0 {
		t.Fatalf("expected zero LLM calls on a cache hit, got %d", deps.Client.callCount())
	}
}

func TestCascadeSkippedWhenLoopExplicitlyRequested(t *testing.T) {
	client := &fakeLLMClient{name: "primary", responses: []string{"real loop answer"}}
	deps := newTestKernel(t, client)
	goal := "summarize our approach about memory retrieval tests"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: goal, Outcome: "success", Summary: goal, Output: "cached-answer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, restore := deps.Kernel.ApplyRequestOverrides(context.Background(), "", "", "on", "", "")
	defer restore()

	out, err := deps.Kernel.Execute(ctx, goal)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "cached-answer" {
		t.Fatal("an explicit /loop request must not be silently answered by the cognition cascade")
	}
	if client.callCount() == 0 {
		t.Fatal("expected the request to reach the model instead of short-circuiting on stale cached facts")
	}
}
