package orchestrator

// escalate.go — climbing the ladder mid-run, on evidence.
//
// router.EntryEffort picks a starting strategy before any model call. This file
// is the other half: reacting when what actually happened says the choice was
// too low. Together they replace the intent classifier, which spent a call
// guessing the same thing up front and could not revise it afterwards.
//
// Only one escalation per run. The signals here mean "this shape of attempt is
// not working", and a second climb would be answering that with more of what
// already failed — at multiplied cost on a metered tier.

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/kernel/loop"
	"github.com/darkcode/kernel/plan"
	"github.com/darkcode/kernel/router"
)

// confidenceScorer is stateless (see router.ConfidenceScorer), so one
// package-level instance is shared across every run rather than allocated
// per call.
var confidenceScorer = router.NewConfidenceScorer()

// logConfidence records how confident a run's final answer reads, per
// router.ConfidenceScorer.
//
// This is observability only. It deliberately does NOT feed the escalation
// ladder above: the scorer is a keyword heuristic (ordinary hedge phrases
// like "I think the best approach is" cost the same penalty whether the
// answer is right or wrong), and escalating on it would mean triggering
// strictly more expensive work — a second, independent escalation trigger
// alongside SignalStuck — on a signal nobody has validated actually
// correlates with wrong answers on this tool's own traffic. That validation
// is exactly what kernel/eval/agent (Phase 4) exists to do once a live model
// is available to run it against; until then, scoring and surfacing the
// number is the honest way to close "built, tested, never connected"
// without smuggling in an unvalidated behavior change.
func (k *Kernel) logConfidence(output string) {
	if strings.TrimSpace(output) == "" {
		return
	}
	score := confidenceScorer.Score(output)
	detail := fmt.Sprintf("Answer confidence: %.2f", score)
	k.log("confidence", detail)
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("confidence", fmt.Sprintf("%.2f", score), detail)
	}
}

// escalateStuckLoop re-runs a stuck loop as a decomposed graph.
//
// A loop gives up after the same call fails four times, and the honest report
// of that — "the agent got stuck and stopped" — is still a spent request that
// produced nothing. The failure is nearly always that the task was too big to
// attack in one piece, which is precisely what decomposing fixes. Retrying the
// loop unchanged is the one thing guaranteed not to help.
//
// Returns ok=false when there is nothing better to try, in which case the
// caller keeps the loop's own output rather than losing it.
//
// rawGoal is the bare user ask (pre-injectProjectContext), passed separately
// from userGoal (the plan/workflow-baked version) so the retry loop below
// can route recall/plan/workflow through Assemble's Injections pool instead
// of baking them into the goal message — see kernel_execute.go's rawGoal
// comment and memory_recorder.go's loopInjections. deepPlan keeps using the
// baked userGoal: it's a one-shot call outside Assemble, so there's nothing
// for the directive to compete against there.
func (k *Kernel) escalateStuckLoop(ctx context.Context, rawGoal, userGoal, recallBlock string, complexity int) (string, *loop.Result, bool) {
	next, why, ok := router.Next(router.EffortLoop, router.SignalStuck)
	if !ok {
		return "", nil, false
	}
	// Escalating to a decomposed graph roughly multiplies cost (a planning
	// call plus a worker call per task, versus the loop's single stream) —
	// the ladder's own package comment already says as much ("at multiplied
	// cost on a metered tier"). If the session's configured spend cap is
	// already reached, that multiplication is the wrong trade regardless of
	// whether it might fix the task: cost_limit_per_day_usd exists precisely
	// to bound spend, and an escalation the governor would have warned about
	// anyway shouldn't happen silently just because it's mid-run rather than
	// a fresh request.
	if reason, damped := k.costDampensEscalation(); damped {
		k.log("escalate", "Not escalating — "+reason)
		return "", nil, false
	}
	k.announceEscalation(router.EffortLoop, next, why)

	depth := decidePlanDepth(userGoal, complexity, false, k.planDepthCfg())
	g, err := k.deepPlan(ctx, k.injectRecall(userGoal, recallBlock), depth)
	if err != nil {
		// The escalation itself failed. Say so and fall back rather than
		// turning a bad answer into no answer.
		k.log("escalate", "Decomposition failed: "+err.Error()+" — keeping the loop's partial result")
		return "", nil, false
	}
	g.Goal = userGoal

	// De-escalate immediately if the plan came back as a single task: the graph
	// machinery has nothing to schedule, and running it anyway is how the
	// ladder turns into a ratchet.
	if len(g.Nodes) <= 1 {
		if back, backWhy, ok := router.Next(router.EffortGraph, router.SignalPlanIsSingleNode); ok {
			k.announceEscalation(router.EffortGraph, back, backWhy)
		}
		return "", nil, false
	}

	k.log("escalate", fmt.Sprintf("Retrying as a %d-task graph", len(g.Nodes)))
	if k.emitter != nil {
		k.emitter.EmitDAGUpdate(g.ToDAG().Summary())
	}

	contract := k.contractFor(g)
	res, err := k.agenticLoop.RunWithInjections(ctx, rawGoal, nil, contract, k.loopInjections(recallBlock))
	if err != nil || res == nil {
		return "", nil, false
	}
	k.mu.Lock()
	k.lastRunPlan = g
	k.mu.Unlock()
	markGraphFrom(g, res.Verdict, res.Completed)

	out := res.Output
	if summary := acceptanceSummary(g); summary != "" {
		out += summary
	}
	return out, res, true
}

// announceEscalation reports a strategy change and the verb that would have
// selected it directly.
//
// A silent change is indistinguishable from a bug when the cost or latency
// jumps. Naming the verb is also how the verbs get found: someone who watches
// the tool reach for /graph learns to type it themselves next time, for a task
// whose shape they already recognise.
func (k *Kernel) announceEscalation(from, to router.Effort, why string) {
	msg := fmt.Sprintf("Started at %s. %s — moving to %s", from, why, to)
	if v := to.Verb(); v != "" {
		msg += fmt.Sprintf(" (/%s next time)", v)
	}
	k.log("escalate", msg)
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("strategy", string(to), msg)
	}
}

// lastRunPlanSnapshot returns the graph the most recent run executed, if any.
func (k *Kernel) lastRunPlanSnapshot() *plan.Graph {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lastRunPlan
}
