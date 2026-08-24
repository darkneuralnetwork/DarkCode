package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/plan"
	"github.com/darkcode/memory/checkpoint"
)

// unfixableArtifactGraph returns a graph whose acceptance check (an artifact
// that never gets created) can never pass, however many repair rounds run —
// the fake LLM's replies are plain text, no tool calls, so nothing in the
// workspace changes during repair. This forces repairFailedAcceptance's
// rollback path deterministically, without needing a real terminal tool.
func unfixableArtifactGraph(goal string) *plan.Graph {
	return &plan.Graph{Goal: goal, Nodes: []*plan.Node{
		{ID: "T1", Status: core.TaskCompleted, Artifacts: []string{"never-created.txt"}},
	}}
}

// TestRepairAutoRollsBackWhenAcceptanceStaysFailed is the regression test for
// Phase 3: a change whose own verification fails must not sit in the working
// tree waiting for a human to notice. Simulates a node that wrote a file, then
// failed its (unfixable) acceptance check — after repair exhausts its rounds,
// the file must be gone, restoring the pre-run checkpoint.
func TestRepairAutoRollsBackWhenAcceptanceStaysFailed(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"looked into it", "tried again", "still stuck"}}
	deps := newTestKernel(t, client)

	workspace := t.TempDir()
	mgr, err := checkpoint.New(t.TempDir(), workspace)
	if err != nil {
		t.Fatalf("checkpoint.New: %v", err)
	}
	deps.Kernel.SetCheckpoints(mgr)

	ctx := context.WithValue(context.Background(), core.WorkspaceKey, workspace)

	preID, ok := deps.Kernel.snapshotBeforeGraph("before test run")
	if !ok {
		t.Fatal("snapshotBeforeGraph did not take a checkpoint")
	}

	// Simulate the node's own (now-failing) work: a file it wrote that
	// shouldn't have been kept once acceptance never passes.
	written := filepath.Join(workspace, "created-by-node.txt")
	if err := os.WriteFile(written, []byte("node output"), 0644); err != nil {
		t.Fatal(err)
	}

	g := unfixableArtifactGraph("do the thing")
	merged := deps.Kernel.repairFailedAcceptance(ctx, g, "original merged output", preID, ok)

	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Errorf("created-by-node.txt still exists after auto-rollback (err=%v) — the workspace was not reverted", err)
	}
	if !strings.Contains(merged, "reverted") {
		t.Errorf("merged output does not mention the rollback: %q", merged)
	}
	// Repair really did run (not skipped) — the fake client's first two
	// scripted replies are consumed, one per round.
	if n := client.callCount(); n < maxRepairRounds {
		t.Errorf("repair ran %d model call(s), want at least %d (maxRepairRounds)", n, maxRepairRounds)
	}
}

// TestRepairDoesNotRollBackOnSuccess proves the happy path is untouched: when
// repair (or the original work) actually satisfies acceptance, nothing is
// reverted and the checkpoint is left alone.
func TestRepairDoesNotRollBackOnSuccess(t *testing.T) {
	client := &fakeLLMClient{responses: []string{"done"}}
	deps := newTestKernel(t, client)

	workspace := t.TempDir()
	mgr, err := checkpoint.New(t.TempDir(), workspace)
	if err != nil {
		t.Fatalf("checkpoint.New: %v", err)
	}
	deps.Kernel.SetCheckpoints(mgr)
	ctx := context.WithValue(context.Background(), core.WorkspaceKey, workspace)

	preID, ok := deps.Kernel.snapshotBeforeGraph("before test run")
	if !ok {
		t.Fatal("snapshotBeforeGraph did not take a checkpoint")
	}

	// The artifact already exists — acceptance passes with nothing to repair.
	kept := filepath.Join(workspace, "kept.txt")
	if err := os.WriteFile(kept, []byte("real work"), 0644); err != nil {
		t.Fatal(err)
	}
	g := &plan.Graph{Goal: "do the thing", Nodes: []*plan.Node{
		{ID: "T1", Status: core.TaskCompleted, Artifacts: []string{"kept.txt"}},
	}}

	merged := deps.Kernel.repairFailedAcceptance(ctx, g, "original merged output", preID, ok)

	if _, err := os.Stat(kept); err != nil {
		t.Errorf("kept.txt was removed even though acceptance passed: %v", err)
	}
	if strings.Contains(merged, "reverted") {
		t.Errorf("a passing run reported a rollback: %q", merged)
	}
	if client.callCount() != 0 {
		t.Errorf("repair called the model %d time(s) for a run that never needed repair", client.callCount())
	}
}
