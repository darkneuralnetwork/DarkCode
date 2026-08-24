package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkcode/infra/core"
)

// Executor runs one goal end to end and returns its final answer. Satisfied
// directly by *orchestrator.Kernel.Execute — no adapter needed, so the thing
// under test in a real eval-agent run is the exact same entrypoint a user's
// turn goes through, not a stand-in for it.
type Executor interface {
	Execute(ctx context.Context, goal string) (string, error)
}

// Score is one executor's result over a corpus — the trajectory-eval
// counterpart of kernel/eval.Score.
type Score struct {
	Adapter string
	Tasks   int
	Passed  int
	// PassRate is Passed/Tasks — the one number a report leads with.
	PassRate float64
	// Failures maps task id to why it failed (missing/empty artifact, or the
	// executor's own error), so the aggregate never hides which cases to
	// read — same reasoning as kernel/eval.Score.Misses.
	Failures map[string]string
}

// Run executes every task in c against exec, each in its own scratch
// subdirectory of workspaceRoot so tasks never interfere with each other's
// artifacts, and scores pass/fail by the same artifact-existence check
// kernel/orchestrator's acceptance checker uses (contract.go): exists and is
// non-empty.
func Run(ctx context.Context, name string, c *Corpus, exec Executor, workspaceRoot string) (Score, error) {
	s := Score{Adapter: name, Tasks: len(c.Tasks), Failures: map[string]string{}}
	for _, t := range c.Tasks {
		ws := filepath.Join(workspaceRoot, t.ID)
		if err := os.MkdirAll(ws, 0755); err != nil {
			return Score{}, fmt.Errorf("task %s: creating scratch workspace: %w", t.ID, err)
		}
		taskCtx := context.WithValue(ctx, core.WorkspaceKey, ws)

		if _, err := exec.Execute(taskCtx, t.Goal); err != nil {
			s.Failures[t.ID] = "executor error: " + err.Error()
			continue
		}

		if reason := missingOrEmptyArtifact(ws, t.Artifacts); reason != "" {
			s.Failures[t.ID] = reason
			continue
		}
		s.Passed++
	}
	if s.Tasks > 0 {
		s.PassRate = float64(s.Passed) / float64(s.Tasks)
	}
	return s, nil
}

// missingOrEmptyArtifact returns a human-readable reason when the first
// artifact that isn't right doesn't exist or is empty, "" when all pass —
// deliberately the same two checks (os.Stat existence, size==0) contract.go
// runs for plan.Node.Artifacts, so a task's pass/fail agrees with what a real
// run's acceptance check would have said.
func missingOrEmptyArtifact(workspace string, artifacts []string) string {
	for _, a := range artifacts {
		path := a
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, a)
		}
		info, err := os.Stat(path)
		switch {
		case err != nil:
			return fmt.Sprintf("expected artifact %s does not exist", a)
		case info.Size() == 0:
			return fmt.Sprintf("expected artifact %s exists but is empty", a)
		}
	}
	return ""
}

// Scorecard renders results as a table — mirrors kernel/eval.Scorecard's
// shape so a report reads the same way whichever harness produced it.
func Scorecard(c *Corpus, scores []Score) string {
	var b strings.Builder
	fmt.Fprintf(&b, "corpus: %s — %d task(s)\n", c.Name, len(c.Tasks))
	if c.About != "" {
		fmt.Fprintf(&b, "%s\n", c.About)
	}
	fmt.Fprintf(&b, "\n%-16s %6s %6s %8s\n", "executor", "passed", "tasks", "pass rate")
	for _, s := range scores {
		fmt.Fprintf(&b, "%-16s %6d %6d %8.1f%%\n", s.Adapter, s.Passed, s.Tasks, s.PassRate*100)
	}
	for _, s := range scores {
		if len(s.Failures) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s failures:\n", s.Adapter)
		for _, id := range sortedFailureIDs(s.Failures) {
			fmt.Fprintf(&b, "  %s: %s\n", id, s.Failures[id])
		}
	}
	return b.String()
}
