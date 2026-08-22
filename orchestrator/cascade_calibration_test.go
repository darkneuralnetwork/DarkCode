package orchestrator

// cascade_calibration_test.go — tests for the threshold-calibration feedback
// loop: re-ask detection (negative labels), forced escalation on re-ask,
// adaptive threshold raising, and JSONL persistence of the rung log.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
)

// seedCache adds a successful no-tool episodic entry so rung 1 can answer.
func seedCache(t *testing.T, deps *testKernelDeps, goal, answer string) {
	t.Helper()
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: goal, Outcome: "success", Summary: goal,
		Output: answer, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCascadeReAskForcesEscalation(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary", responses: []string{"fresh-llm-answer"}}
	deps := newTestKernel(t, client)
	goal := "summarize our approach about memory retrieval tests"
	seedCache(t, deps, goal, "cached-answer")

	// First ask: served from the cache, no LLM.
	out, err := deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if out != "cached-answer" || client.callCount() != 0 {
		t.Fatalf("first ask should hit the cache (got %q, %d LLM calls)", out, client.callCount())
	}

	// Immediate re-ask: the cached answer didn't satisfy — must escalate to
	// the LLM instead of replaying the same cache hit.
	out, err = deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if client.callCount() == 0 {
		t.Fatal("re-ask must escalate to the LLM, not re-serve the rejected cache answer")
	}
	if out == "cached-answer" {
		t.Fatal("re-ask returned the rejected cached answer")
	}

	log := deps.Kernel.CascadeLog()
	if len(log) != 2 {
		t.Fatalf("expected 2 cascade entries, got %d", len(log))
	}
	if !log[0].Retried {
		t.Fatal("original cache entry must carry the Retried negative label")
	}
	if log[1].RetryOfRungName != "cache" || log[1].Rung != router.RungLLM {
		t.Fatalf("escalation entry must name the rejected rung: %+v", log[1])
	}

	stats := deps.Kernel.CascadeStats()
	for _, s := range stats {
		if s.Name == "cache" && s.Retried != 1 {
			t.Fatalf("cache rung should record 1 retry, got %+v", s)
		}
	}
}

func TestCascadeThresholdRaisesOnHighRetryRatio(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary"}
	deps := newTestKernel(t, client)

	// Five distinct questions, each answered from the cache then immediately
	// re-asked (rejected). After cascadeMinSamples answers with a retry
	// ratio above cascadeMaxRetryRatio, the cache rung's threshold must rise
	// above the 0.9 confidence its answers carry — disabling it.
	for i := 0; i < 5; i++ {
		goal := fmt.Sprintf("summarize our approach about topic%d in the docs", i)
		seedCache(t, deps, goal, "cached")
		if _, err := deps.Kernel.Execute(context.Background(), goal); err != nil {
			t.Fatal(err)
		}
		if _, err := deps.Kernel.Execute(context.Background(), goal); err != nil {
			t.Fatal(err)
		}
	}

	var cacheStats *CascadeRungStats
	for _, s := range deps.Kernel.CascadeStats() {
		if s.Name == "cache" {
			c := s
			cacheStats = &c
		}
	}
	if cacheStats == nil {
		t.Fatal("no cache rung stats")
	}
	if cacheStats.Threshold <= cascadeDefaultThreshold {
		t.Fatalf("threshold should have risen from %.2f, still %.2f (answered=%d retried=%d)",
			cascadeDefaultThreshold, cacheStats.Threshold, cacheStats.Answered, cacheStats.Retried)
	}

	// With the threshold raised past the cache's 0.9 confidence, a sixth
	// cached question must now escalate straight to the LLM.
	if cacheStats.Threshold > 0.9 {
		goal := "summarize our approach about the final topic in the docs"
		seedCache(t, deps, goal, "cached")
		before := client.callCount()
		if _, err := deps.Kernel.Execute(context.Background(), goal); err != nil {
			t.Fatal(err)
		}
		if client.callCount() == before {
			t.Fatal("cache rung should be disabled after repeated rejections; expected LLM escalation")
		}
	}
}

func TestCascadeLogPersistsJSONL(t *testing.T) {
	deps := newTestKernel(t, nil)
	path := filepath.Join(t.TempDir(), "cascade_log.jsonl")
	deps.Kernel.SetCascadeLogPath(path)

	goal := "summarize our approach about persistence checks"
	seedCache(t, deps, goal, "cached-answer")
	if _, err := deps.Kernel.Execute(context.Background(), goal); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("cascade log not written: %v", err)
	}
	defer f.Close()
	var entries []CascadeEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e CascadeEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad JSONL line: %v", err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 persisted entry, got %d", len(entries))
	}
	if !entries[0].Answered || entries[0].RungName != "cache" {
		t.Fatalf("persisted entry wrong: %+v", entries[0])
	}
}
