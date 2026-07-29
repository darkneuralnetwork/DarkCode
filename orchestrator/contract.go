package orchestrator

// contract.go — turning a plan into a definition of done the loop can enforce.
//
// The plan graph has carried acceptance criteria, expected artifacts and Proof
// slots since it was written, and the DAG path already ran them — but only
// after everything finished, as a report. Loop mode never saw them at all,
// because Execute handed the goal to the loop and returned before the planner
// was ever reached. So the system had two halves of one idea: a path that knew
// what "done" meant but could not act on it, and a path that could iterate but
// had nothing to iterate toward.
//
// This is the join. A plan.Graph becomes a loop.Contract: the criteria are
// shown to the model as the target, and running them is what ends the loop.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/loop"
	"github.com/darkcode/plan"
	"github.com/darkcode/tools"
)

// contractFor builds the loop contract for a whole plan graph: every node's
// criteria and artifacts, with a Verify that runs them and reports the first
// real failure.
//
// Commands are memoised across the graph by checkAcceptance's `ran` map, so a
// plan whose ten nodes all fall back to "go build && go test" pays for one test
// run, not ten.
func (k *Kernel) contractFor(g *plan.Graph) *loop.Contract {
	if g == nil || len(g.Nodes) == 0 {
		return nil
	}
	c := &loop.Contract{}
	for _, n := range g.Nodes {
		c.Criteria = append(c.Criteria, n.Acceptance...)
		c.Artifacts = append(c.Artifacts, n.Artifacts...)
	}
	c.Criteria = dedupNonEmpty(c.Criteria)
	c.Artifacts = dedupNonEmpty(c.Artifacts)

	c.Verify = func(ctx context.Context) loop.Verdict {
		return k.verifyContract(ctx, g)
	}
	return c
}

// verifyContract runs every node's acceptance criteria plus the artifact
// existence checks, and returns the aggregate verdict.
//
// Node Proof is reset before each pass. Verify is called once per correction
// round, and without the reset a node would accumulate one Proof entry per
// attempt — so the Blueprint tab would show a task as both failing and passing,
// with no way to tell which run was the last one.
func (k *Kernel) verifyContract(ctx context.Context, g *plan.Graph) loop.Verdict {
	var v loop.Verdict
	var evidence []string

	ran := map[string]bool{}
	for _, n := range g.Nodes {
		n.Proof = nil
		if !k.checkAcceptance(ctx, n, ran) {
			for _, p := range n.Proof {
				if p.Command != "" && !p.Passed {
					evidence = append(evidence,
						fmt.Sprintf("$ %s\n%s", p.Command, strings.TrimSpace(p.Output)))
				}
			}
		}
		for _, p := range n.Proof {
			if p.Command != "" {
				v.Checked++
			}
		}
	}

	// Artifact existence is checkable without running anything, and it catches
	// the failure the acceptance commands miss entirely: a build that compiles
	// because the agent never created the file it was asked for.
	ws := tools.CurrentWorkspace(ctx)
	if ws == "" {
		ws, _ = os.Getwd()
	}
	for _, n := range g.Nodes {
		for _, a := range n.Artifacts {
			path := a
			if !filepath.IsAbs(path) {
				path = filepath.Join(ws, a)
			}
			v.Checked++
			info, err := os.Stat(path)
			switch {
			case err != nil:
				evidence = append(evidence, fmt.Sprintf("expected artifact %s does not exist", a))
			case info.Size() == 0:
				evidence = append(evidence, fmt.Sprintf("expected artifact %s exists but is empty", a))
			}
		}
	}

	v.Passed = len(evidence) == 0
	v.Evidence = strings.Join(evidence, "\n\n")
	return v
}

// loopPlanMinComplexity is the complexity below which loop mode skips
// planning. Decomposing "rename this variable" into a task graph costs a
// planner call and produces one node whose acceptance criterion is the default
// build-and-test — the self-evaluation fallback is both cheaper and no worse.
const loopPlanMinComplexity = 4

// shouldPlanForLoop decides whether a loop-mode goal is worth planning. An
// active project always is: its plan and workflow are the user's stated
// intent, and ignoring them is how "follow the blueprint" stops meaning
// anything.
func (k *Kernel) shouldPlanForLoop(goal string, complexity int, hasProjectGuidance bool) bool {
	if hasProjectGuidance {
		return true
	}
	if k.readOnlyForRequest() || k.toolsDisabledForRequest() {
		return false // nothing to build, so nothing to verify
	}
	return complexity >= loopPlanMinComplexity
}

func (k *Kernel) planDepthCfg() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cfg.PlanDepth
}

// markGraphFrom records the loop's outcome onto the plan graph so the
// Blueprint tab shows what actually happened. The loop works the whole graph
// as one unit rather than node-by-node, so a proven verdict completes every
// node and a failure leaves them all pending — an honest, if coarse, status.
func markGraphFrom(g *plan.Graph, v loop.Verdict, completed bool) {
	status := core.TaskPending
	if v.Proven() || (completed && v.Checked == 0) {
		status = core.TaskCompleted
	} else if v.Checked > 0 && !v.Passed {
		status = core.TaskFailed
	}
	for _, n := range g.Nodes {
		n.Status = status
	}
}

// acceptanceSummary renders the proof already attached to the graph by
// verifyContract. It does NOT re-run anything: the checks were run to decide
// whether the loop could stop, and running them again to print them would
// double the cost of every verified task.
func acceptanceSummary(g *plan.Graph) string {
	var lines []string
	checked, failed := 0, 0
	for _, n := range g.Nodes {
		for _, p := range n.Proof {
			if p.Command == "" {
				continue
			}
			checked++
			mark := "✓"
			if !p.Passed {
				mark = "✗"
				failed++
			}
			lines = append(lines, fmt.Sprintf("- %s `%s`", mark, p.Command))
		}
	}
	if checked == 0 {
		return ""
	}
	head := fmt.Sprintf("\n\n**Acceptance checks** — %d run", checked)
	if failed > 0 {
		head += fmt.Sprintf(", %d failing", failed)
	} else {
		head += ", all passing"
	}
	return head + ":\n" + strings.Join(dedupNonEmpty(lines), "\n")
}

func dedupNonEmpty(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
