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

	ran := map[string]plan.Proof{}
	reported := map[string]bool{}
	for _, n := range g.Nodes {
		n.Proof = nil
		k.checkAcceptance(ctx, n, ran)
		for _, p := range n.Proof {
			if p.Command == "" || reported[p.Command] {
				continue // prose is not evidence; a shared command is one check
			}
			reported[p.Command] = true
			// Counted per distinct COMMAND, not per node. checkAcceptance
			// attaches a memoised result to every node that depends on it, so
			// counting occurrences would report ten checks for a ten-node plan
			// that ran one test suite.
			v.Checked++
			if !p.Passed {
				evidence = append(evidence,
					fmt.Sprintf("$ %s\n%s", p.Command, strings.TrimSpace(p.Output)))
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

// untilContract turns a user-stated criterion into an enforceable contract,
// running shell criteria through the tool registry so the sandbox, the
// permission gate and the circuit breaker all still apply — a stop condition
// must not become a way to run a command that a tool call could not.
func (k *Kernel) untilContract(ctx context.Context, criterion string) *loop.Contract {
	ws := tools.CurrentWorkspace(ctx)
	if ws == "" {
		ws, _ = os.Getwd()
	}
	run := func(cmd string) (bool, string) {
		if k.registry == nil {
			return false, "no tool registry available to check the criterion"
		}
		runCtx, cancel := context.WithTimeout(ctx, acceptanceTimeout)
		defer cancel()
		res, err := k.registry.Execute(runCtx, "terminal", map[string]interface{}{
			"command": cmd, "workdir": ws,
		})
		switch {
		case err != nil:
			return false, err.Error()
		case res == nil:
			return false, "the check produced no result"
		default:
			return res.Success, strings.TrimSpace(res.Output + " " + res.Error)
		}
	}
	return loop.ContractFromUntil(criterion, ws, run)
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
	// An explicit /graph or /loop decides this outright — the user has said
	// which shape of work they want, and second-guessing them from a
	// complexity heuristic is how the verb stops meaning anything.
	if force, set := k.planForced(); set {
		return force
	}
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
// Blueprint tab shows what actually happened.
//
// Status is per node, decided by that node's own evidence. An earlier version
// stamped every node identically because the loop works the graph as one unit,
// which made the Blueprint tab useless in exactly the case it matters: a run
// where three tasks passed and one failed showed four failures, so there was
// nothing to look at to find out which one to go and fix.
//
// The loop does run the graph as a whole, so the attribution is not free — but
// it does not have to be guessed. verifyContract already attaches Proof to the
// node whose criteria produced it, and node.Artifacts is checkable on its own.
// A node with failing proof failed; a node with passing proof completed; a node
// with neither is only as good as the run around it, and says so.
func markGraphFrom(g *plan.Graph, v loop.Verdict, completed bool) {
	// Fallback for nodes that carried nothing checkable of their own. The run
	// is the only evidence they have, which is weaker than proof but still
	// better than reporting the whole graph by its worst node.
	unproven := core.TaskPending
	switch {
	case v.Proven() || (completed && v.Checked == 0):
		unproven = core.TaskCompleted
	case v.Checked > 0 && !v.Passed:
		// Something in this graph failed, but not necessarily this node.
		// Pending, not failed: blaming a node whose own checks never ran is
		// how a red tick stops meaning anything, the same way a green one does.
		unproven = core.TaskPending
	}

	for _, n := range g.Nodes {
		switch nodeProofStatus(n) {
		case core.TaskFailed:
			n.Status = core.TaskFailed
		case core.TaskCompleted:
			n.Status = core.TaskCompleted
		default:
			n.Status = unproven
		}
	}
}

// nodeProofStatus reads a node's own evidence: failed if any machine-checkable
// criterion failed, completed if at least one passed, and pending when the node
// had nothing runnable to say either way.
//
// Prose criteria are deliberately not evidence. checkAcceptance records them as
// "not machine-checkable", and counting an unverifiable sentence as a pass is
// the precise failure this whole path exists to remove.
func nodeProofStatus(n *plan.Node) core.TaskStatus {
	passed := false
	for _, p := range n.Proof {
		if p.Command == "" {
			continue
		}
		if !p.Passed {
			return core.TaskFailed
		}
		passed = true
	}
	if passed {
		return core.TaskCompleted
	}
	return core.TaskPending
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
		// Nothing was mechanically verified, and saying nothing about that is
		// how prose passes as work. A sub-agent that describes an
		// implementation instead of writing it produces a confident answer,
		// a completed node and an empty proof set — the run then reads as
		// success because the only thing that would have contradicted it was
		// silent.
		//
		// Distinguish the two reasons, because the fix differs. No criteria at
		// all means the planner never said what done looks like. Criteria that
		// exist but ran nothing means they were prose a machine cannot check.
		declared := 0
		for _, n := range g.Nodes {
			declared += len(n.Acceptance) + len(n.Artifacts)
		}
		if declared == 0 {
			return "\n\n⚠ **Nothing was verified** — this plan declared no acceptance " +
				"criteria or expected files, so the claim above rests on the model's " +
				"word rather than on anything that was checked."
		}
		return "\n\n⚠ **Nothing was verified** — this plan's acceptance criteria were " +
			"prose rather than commands or file paths, so none could be run. The claim " +
			"above rests on the model's word."
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
