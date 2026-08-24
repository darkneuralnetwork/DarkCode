package agent

// live_test.go wires a REAL *orchestrator.Kernel — the actual thing under
// test — and runs the corpus through it, printing (and floor-checking) the
// scorecard. Deliberately a _test.go file, not part of agent.go/run.go's
// own import graph: Go excludes test files from what a normal importer of
// this package pulls in, so kernel/eval/agent stays a lean library
// (infra/core + stdlib only) for anything that just wants Corpus/Run/Score,
// while this file can freely depend on orchestrator/router/tools/memory/
// ctxengine/llm without bloating that graph.
//
// Skipped unless DARKCODE_EVAL_AGENT_LIVE=1 — this needs a real model and
// spends real tokens, which is exactly why `make eval-agent` is its own
// opt-in target, never part of `test`/`test-race`/`ci` (see agent.go's
// package comment). Configure the model via the same DARKCODE_API_KEY/
// DARKCODE_BASE_URL/DARKCODE_MODEL env vars the CLI itself reads.

import (
	"context"
	"os"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/orchestrator"
	"github.com/darkcode/kernel/router"
	"github.com/darkcode/memory/ctxengine"
	"github.com/darkcode/memory/memory"
	"github.com/darkcode/model/llm"
	"github.com/darkcode/tools/tools"
)

func TestEvalAgentLive(t *testing.T) {
	if os.Getenv("DARKCODE_EVAL_AGENT_LIVE") != "1" {
		t.Skip("set DARKCODE_EVAL_AGENT_LIVE=1 to run — this calls a real model and spends real tokens")
	}
	apiKey := os.Getenv("DARKCODE_API_KEY")
	baseURL := os.Getenv("DARKCODE_BASE_URL")
	model := os.Getenv("DARKCODE_MODEL")
	if baseURL == "" || model == "" {
		t.Fatal("DARKCODE_EVAL_AGENT_LIVE=1 requires DARKCODE_BASE_URL and DARKCODE_MODEL (and usually DARKCODE_API_KEY) to also be set")
	}

	c := load(t)

	mem, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("memory.NewSystem: %v", err)
	}
	t.Cleanup(mem.Shutdown)

	client := llm.NewClient(baseURL, apiKey, model)
	rtr := router.NewRouter(core.RouteSingle, nil)
	for _, tier := range []core.ModelTier{core.ModelTierCoding, core.ModelTierReasoning, core.ModelTierFast} {
		rtr.RegisterModel(tier, client, model)
	}
	rtr.MarkPrimary(model)

	reg := tools.NewRegistry()
	tools.RegisterBuiltinTools(reg, mem, rtr, nil)

	comp := ctxengine.NewEngine(nil)
	comp.SetClient(client, model)
	comp.SetRouter(rtr)

	k := orchestrator.New(orchestrator.DefaultConfig(), rtr, reg, mem, comp, nil)

	s, err := Run(context.Background(), model, c, k, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Log("\n" + Scorecard(c, []Score{s}))

	if s.Passed == 0 {
		t.Errorf("every task failed — the harness or the model is broken, not just imperfect: %v", s.Failures)
	}
}
