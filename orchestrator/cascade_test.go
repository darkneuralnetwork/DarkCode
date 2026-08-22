package orchestrator

// cascade_test.go — Phase A acceptance tests (local-first upgrade §7):
// assert rung selection, no LLM escalation on high-confidence local hits,
// and the per-query rung log, using the existing fake-LLM harness.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
	"github.com/darkcode/tools/deterministic"
)

// writeGoWorkspace creates a tiny Go workspace for the deterministic tools.
func writeGoWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `package main

// ParseConfig loads the config.
func ParseConfig(path string) error { return nil }

func main() { _ = ParseConfig("x") }
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCascade_Rung0_DeterministicAnswersWithoutLLM(t *testing.T) {
	deps := newTestKernel(t, nil)
	deterministic.RegisterAll(deps.Registry)
	ws := writeGoWorkspace(t)
	ctx := context.WithValue(context.Background(), core.WorkspaceKey, ws)

	out, err := deps.Kernel.Execute(ctx, "where is ParseConfig defined?")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deps.Client.callCount() != 0 {
		t.Fatalf("expected zero LLM calls for a structural question, got %d", deps.Client.callCount())
	}
	if !containsAll(out, "ParseConfig", "main.go") {
		t.Fatalf("answer missing citation: %s", out)
	}

	log := deps.Kernel.CascadeLog()
	if len(log) != 1 {
		t.Fatalf("expected 1 cascade entry, got %d", len(log))
	}
	e := log[0]
	if !e.Answered || e.Rung != router.RungDeterministic || e.EntryRung != router.RungDeterministic {
		t.Fatalf("wrong cascade entry: %+v", e)
	}
	if e.Confidence.Score < 1.0 {
		t.Fatalf("deterministic answers are binary-confident, got %f", e.Confidence.Score)
	}
}

func TestCascade_Rung1_CacheHitSkipsLLM(t *testing.T) {
	deps := newTestKernel(t, nil)

	// Seed the answer cache: a prior successful no-tool task.
	goal := "summarize our approach about memory retrieval tests"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal:  goal,
		Outcome:   "success",
		Summary:   goal,
		Output:    "cached-answer",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "cached-answer" {
		t.Fatalf("expected cached answer, got %q", out)
	}
	if deps.Client.callCount() != 0 {
		t.Fatalf("expected zero LLM calls on cache hit, got %d", deps.Client.callCount())
	}
	log := deps.Kernel.CascadeLog()
	if len(log) != 1 || log[0].Rung != router.RungCache || !log[0].Answered {
		t.Fatalf("wrong cascade entry: %+v", log)
	}
}

func TestCascade_Rung2_GraphAnswersWhenRung0Misses(t *testing.T) {
	// No deterministic tools registered and no Go workspace → rung 0 misses;
	// the seeded KG fact must answer at rung 2 with a citation.
	deps := newTestKernel(t, nil)
	kg := deps.Memory.KG()
	if err := kg.AddNode(&core.KGNode{
		ID: "symbol:LoadState@state/store.go", Label: "LoadState", Type: core.KGNodeSymbol,
		Properties: map[string]string{"kind": "function", "origin": "code_index", "references": "3"},
		Provenance: "state/store.go:41", Confidence: 1, LastSeen: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := deps.Kernel.Execute(context.Background(), "where is LoadState defined?")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deps.Client.callCount() != 0 {
		t.Fatalf("expected zero LLM calls for a graph-answerable question, got %d", deps.Client.callCount())
	}
	if !containsAll(out, "LoadState", "state/store.go:41") {
		t.Fatalf("answer missing citation: %s", out)
	}
	log := deps.Kernel.CascadeLog()
	if len(log) != 1 || log[0].Rung != router.RungGraph || !log[0].Answered {
		t.Fatalf("wrong cascade entry: %+v", log)
	}
	if len(log[0].Confidence.Provenance) == 0 {
		t.Fatal("graph answer must carry provenance")
	}
}

func TestCascade_EscalatesToLLMOnMiss(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary", responses: []string{"llm-answer"}}
	deps := newTestKernel(t, client)

	out, err := deps.Kernel.Execute(context.Background(), "write a function that reverses a linked list in go")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if client.callCount() == 0 {
		t.Fatal("expected the LLM path to run for a synthesis request")
	}
	if out == "" {
		t.Fatal("expected an LLM answer")
	}

	log := deps.Kernel.CascadeLog()
	if len(log) != 1 {
		t.Fatalf("expected 1 cascade entry, got %d", len(log))
	}
	e := log[0]
	if e.Answered || e.Rung != router.RungLLM {
		t.Fatalf("expected an escalation entry, got %+v", e)
	}
	// Synthesis/action intent must enter at the LLM rung (never risk a
	// confidently-wrong cache hit for a "write/create/fix" request).
	if e.EntryRung != router.RungLLM {
		t.Fatalf("synthesis request should enter at the LLM rung, got %d", e.EntryRung)
	}
}

func TestCascade_ActionRequestNeverServedFromRetrieval(t *testing.T) {
	// Even with a byte-identical past success in the cache, an action-shaped
	// request must reach the LLM/tool path: the entry classifier routes it
	// to rung 4 directly.
	client := &fakeLLMClient{name: "fake-primary", responses: []string{"did the work"}}
	deps := newTestKernel(t, client)
	goal := "create a config file for the linter in the repo root"
	if err := deps.Memory.EpisodicAdd(core.EpisodicEntry{
		TaskGoal: goal, Outcome: "success", Summary: goal,
		Output: "stale-cached-side-effect-claim", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "stale-cached-side-effect-claim" {
		t.Fatal("action request was served from the cache — side effects would be silently skipped")
	}
	if client.callCount() == 0 {
		t.Fatal("expected the LLM path to run")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
