package orchestrator

// repair.go — acting on a failed acceptance check instead of reporting it.
//
// The DAG path has always been able to prove a task wasn't finished: the
// planner emits acceptance criteria, verifyAcceptance runs them, and a failing
// command produces a compiler error or a red test with the exact reason in it.
// What it did with that proof was print it under the answer and return.
//
// That is the exact mirror of what was wrong with loop mode. The loop could
// iterate indefinitely but had no definition of done, so it stopped on the
// model's say-so. The DAG had a rigorous definition of done but no way to
// iterate, so it stopped with the evidence of failure in its hand. Neither was
// missing capability — they were each missing the other's half.
//
// So a failing check now goes back through the loop, with the failing command's
// real output as the correction signal. That is the whole idea: the thing that
// knows the work is broken hands it to the thing that can fix it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/loop"
	"github.com/darkcode/plan"
)

// maxRepairRounds bounds the repair attempts for one graph. Each round is a
// full loop run plus a re-check, so this is the expensive budget in the
// pipeline. Two is enough for the common case — a missed import, a stale
// snapshot test — and a third round of the same failure is a signal the agent
// cannot fix it, not a reason to pay again.
const maxRepairRounds = 2

// repairFailedAcceptance runs the graph's acceptance criteria and, when a
// machine-checkable one fails, hands the failure to the ReAct loop to fix.
// Returns the merged answer with the final evidence appended.
func (k *Kernel) repairFailedAcceptance(ctx context.Context, g *plan.Graph, merged string) string {
	if g == nil || len(g.Nodes) == 0 {
		return merged
	}

	verdict := k.verifyContract(ctx, g)
	if verdict.Checked == 0 {
		return merged // nothing was checkable; there is nothing to repair toward
	}

	for round := 1; verdict.Checked > 0 && !verdict.Passed && round <= maxRepairRounds; round++ {
		if ctx.Err() != nil {
			break
		}
		if k.agenticLoop == nil {
			break // repair needs the loop; without it this stays a report
		}
		k.log("repair", fmt.Sprintf("Acceptance failed — repair round %d/%d", round, maxRepairRounds))
		if k.emitter != nil {
			k.emitter.EmitTaskUpdate("repair", "running",
				fmt.Sprintf("Acceptance checks failed — repairing (round %d/%d)", round, maxRepairRounds))
		}

		goal := repairGoal(g.Goal, verdict.Evidence)
		// No contract on the repair run itself: the caller re-checks below, and
		// giving the loop the same Verify would make it run the whole suite on
		// every internal correction round as well as here — the same evidence,
		// paid for twice.
		res, err := k.agenticLoop.Run(ctx, goal, nil)
		if err != nil {
			k.log("repair", "Repair run failed: "+err.Error())
			break
		}
		if strings.TrimSpace(res.Output) != "" {
			merged += "\n\n**Repair round " + fmt.Sprint(round) + "**\n" + res.Output
		}

		verdict = k.verifyContract(ctx, g)
		if verdict.Passed {
			k.log("repair", "Acceptance checks pass after repair")
			if k.emitter != nil {
				k.emitter.EmitTaskUpdate("repair", "completed", "Acceptance checks pass after repair")
			}
			break
		}
	}

	// Node status must reflect the evidence, not the sub-agent's opinion. A
	// node that reported success and whose acceptance check is still failing is
	// not complete, and marking it so is how a Blueprint tab full of green
	// ticks stops meaning anything.
	if verdict.Checked > 0 && !verdict.Passed {
		for _, n := range g.Nodes {
			if n.Status == core.TaskCompleted && hasFailingProof(n) {
				n.Status = core.TaskFailed
			}
		}
	}

	if summary := acceptanceSummary(g); summary != "" {
		merged += summary
	}
	return merged
}

func hasFailingProof(n *plan.Node) bool {
	for _, p := range n.Proof {
		if p.Command != "" && !p.Passed {
			return true
		}
	}
	return false
}

// repairGoal frames the failure for the loop. The failing command's own output
// is the payload — a compiler already says precisely what is wrong, and
// paraphrasing it into "please fix the build" throws away the only part the
// model actually needs.
func repairGoal(original, evidence string) string {
	var b strings.Builder
	b.WriteString("The work for this task is complete except that its acceptance checks are FAILING.\n\n")
	b.WriteString("Original goal: ")
	b.WriteString(original)
	b.WriteString("\n\nFailing checks and their real output:\n\n")
	b.WriteString(strutil.Truncate(evidence, loop.MaxObservationLen))
	b.WriteString("\n\nDiagnose the cause and fix it. Change the code, not the test, unless the " +
		"test is provably wrong. Do not restate the plan or re-do work that already succeeded.")
	return b.String()
}
