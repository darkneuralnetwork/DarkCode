package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/metrics"
	"github.com/darkcode/kernel/router"
)

// singleTaskPlanJSON is a decomposition that did not decompose anything.
const singleTaskPlanJSON = `[{"name":"task1","goal":"do the whole thing","agent":"worker","deps":[]}]`

// TestStuckLoopEscalatesToAGraph. Before this, a loop that gave up returned
// "the agent got stuck…" and the request was spent for nothing. The cause is
// nearly always a task too big to attack in one piece, so the escalation has to
// change the shape of the attempt rather than repeat it.
func TestStuckLoopEscalatesToAGraph(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{testPlanJSON, "worked"}}
	deps := newTestKernel(t, client)

	out, res, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rewrite the parser", "rewrite the parser", "", 5)
	if !ok {
		t.Fatal("a stuck loop did not escalate to a graph")
	}
	if res == nil || strings.TrimSpace(out) == "" {
		t.Fatal("escalation reported success with nothing to show for it")
	}
	// The graph it ran must be the one recorded, so the Blueprint tab and the
	// acceptance summary describe the attempt that actually happened.
	g := deps.Kernel.lastRunPlanSnapshot()
	if g == nil {
		t.Fatal("escalation did not record the plan it ran")
	}
	if len(g.Nodes) != 2 {
		t.Errorf("recorded plan has %d nodes, want the 2 it decomposed into", len(g.Nodes))
	}
}

// TestSingleNodePlanDeEscalates. Escalation without de-escalation ratchets: the
// run would pay for graph machinery that has nothing to schedule.
func TestSingleNodePlanDeEscalates(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{singleTaskPlanJSON, "worked"}}
	deps := newTestKernel(t, client)

	out, _, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rename a field", "rename a field", "", 5)
	if ok {
		t.Error("a one-task plan was executed as a graph anyway")
	}
	if out != "" {
		t.Errorf("de-escalated path produced graph output: %q", out)
	}
	// One call to plan, and no execution after it.
	if n := client.callCount(); n != 1 {
		t.Errorf("spent %d calls; want 1 (the plan) with nothing run after it", n)
	}
	if !loggedContains(deps.Kernel, "one task") {
		t.Error("de-escalation was not announced")
	}
}

// TestEscalationAnnouncesItself. A silent strategy change is indistinguishable
// from a bug when the cost or latency jumps, and the announcement is how the
// verbs get discovered.
func TestEscalationAnnouncesItself(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)

	deps.Kernel.announceEscalation(router.EffortLoop, router.EffortGraph, "the same step keeps failing")

	if !loggedContains(deps.Kernel, "/graph") {
		t.Error("announcement does not name the verb that would select it directly")
	}
	if !loggedContains(deps.Kernel, "keeps failing") {
		t.Error("announcement does not say why the strategy changed")
	}
	if !loggedContains(deps.Kernel, "loop") {
		t.Error("announcement does not say what it started as")
	}
}

// TestEscalationSurvivesAFailedPlanner. Turning a partial answer into no answer
// is worse than the stuck report it was trying to improve on.
func TestEscalationSurvivesAFailedPlanner(t *testing.T) {
	client := &fakeLLMClient{name: "fake", err: context.DeadlineExceeded}
	deps := newTestKernel(t, client)

	out, res, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rewrite the parser", "rewrite the parser", "", 5)
	if ok {
		t.Error("escalation claimed success with an unreachable planner")
	}
	if out != "" || res != nil {
		t.Errorf("failed escalation produced output: %q", out)
	}
}

// TestSetSelfCritiqueReachesTheLoop is the wiring regression test for Phase
// 6: Kernel.SetSelfCritique must actually reach the ReActLoop it holds, off
// by default until an explicit call turns it on — same shape as
// TestSetCostGovernorReachesTheLoop, for a different setter.
func TestSetSelfCritiqueReachesTheLoop(t *testing.T) {
	deps := newTestKernel(t, &fakeLLMClient{name: "fake"})

	if deps.Kernel.agenticLoop == nil {
		t.Fatal("the kernel has no loop, so nothing could be wired")
	}
	if deps.Kernel.agenticLoop.SelfCritiqueEnabled() {
		t.Error("self-critique was on by default; it must stay off until explicitly enabled")
	}

	deps.Kernel.SetSelfCritique(true)
	if !deps.Kernel.agenticLoop.SelfCritiqueEnabled() {
		t.Error("SetSelfCritique(true) did not reach the loop")
	}

	deps.Kernel.SetSelfCritique(false)
	if deps.Kernel.agenticLoop.SelfCritiqueEnabled() {
		t.Error("SetSelfCritique(false) did not reach the loop")
	}
}

// TestLogConfidenceScoresAndSurfacesTheAnswer is the regression test for
// Phase 5's confidence-scoring wiring: router.ConfidenceScorer previously had
// zero callers anywhere in the codebase (the same "built, tested, never
// connected" shape as ctxfit's importance scorer from Phase 1). It must now
// actually run on a run's output and the result must be visible — in the
// task log, and to any GUI/CLI listening on the emitter — without touching
// the escalation ladder itself (see logConfidence's doc comment for why it
// deliberately stops at observability).
func TestLogConfidenceScoresAndSurfacesTheAnswer(t *testing.T) {
	deps := newTestKernel(t, &fakeLLMClient{name: "fake"})

	deps.Kernel.logConfidence("Here is the answer: 42.")

	if !loggedContains(deps.Kernel, "confidence") {
		t.Error("logConfidence did not log anything — router.ConfidenceScorer is still uncalled in practice")
	}

	// An empty answer has nothing to score and must not log a bogus number.
	before := len(deps.Kernel.GetTaskLog())
	deps.Kernel.logConfidence("   ")
	if len(deps.Kernel.GetTaskLog()) != before {
		t.Error("logConfidence logged something for an empty answer")
	}
}

// loggedContains reports whether any kernel log line contains sub.
func loggedContains(k *Kernel, sub string) bool {
	for _, e := range k.GetTaskLog() {
		if strings.Contains(e.Detail, sub) {
			return true
		}
	}
	return false
}

// TestSetCostGovernorReachesTheLoop. The check is installed on the loop the
// kernel already holds, so wiring order matters: if the governor were set
// before the loop existed, the per-iteration check would silently never be
// installed and the cap would go back to being a once-per-request gate.
func TestSetCostGovernorReachesTheLoop(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)

	if deps.Kernel.agenticLoop == nil {
		t.Fatal("the kernel has no loop, so nothing could be wired")
	}

	// A cap that is already exceeded: any check must refuse.
	gov := metrics.NewCostGovernor(metrics.Default, metrics.BudgetLimits{
		PerSessionUSD: 0.0000001,
		Action:        metrics.BudgetActionBlock,
	})
	deps.Kernel.SetCostGovernor(gov)

	if deps.Kernel.agenticLoop.BudgetCheckInstalled() != true {
		t.Error("SetCostGovernor did not install a per-iteration check on the loop")
	}

	// And clearing it removes the check rather than leaving a stale one.
	deps.Kernel.SetCostGovernor(nil)
	if deps.Kernel.agenticLoop.BudgetCheckInstalled() {
		t.Error("clearing the governor left the loop's check installed")
	}
}

// TestCostCapDampensEscalation is the regression test for Phase 5's
// cost-aware escalation: a stuck loop must not decompose into a (roughly
// more expensive) graph once the configured spend cap is already reached,
// even though the signal that would normally trigger it (SignalStuck) is
// present. Same stuck-loop setup as TestStuckLoopEscalatesToAGraph, but with
// an already-exceeded cost governor installed first.
func TestCostCapDampensEscalation(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{testPlanJSON, "worked"}}
	deps := newTestKernel(t, client)

	// An isolated tracker (not the shared metrics.Default, whose accumulated
	// cost depends on what other tests in this process happened to run) with
	// a known cost recorded directly, so the cap is deterministically
	// exceeded regardless of test order.
	tracker := metrics.NewUsageTracker()
	tracker.Record(metrics.RequestRecord{Cost: 5.00})
	gov := metrics.NewCostGovernor(tracker, metrics.BudgetLimits{
		PerSessionUSD: 1.00,
		Action:        metrics.BudgetActionWarn, // warn, not block — the request itself may still proceed; only the escalation should be damped
	})
	deps.Kernel.SetCostGovernor(gov)

	out, res, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rewrite the parser", "rewrite the parser", "", 5)
	if ok {
		t.Fatal("escalation proceeded despite the spend cap already being reached")
	}
	if out != "" || res != nil {
		t.Errorf("a damped escalation produced output: %q, %+v", out, res)
	}
	// No planning call should have been spent trying to decompose — the cap
	// was checked before deepPlan, not after.
	if n := client.callCount(); n != 0 {
		t.Errorf("client was called %d time(s); a damped escalation must not spend anything", n)
	}
	if !loggedContains(deps.Kernel, "Not escalating") {
		t.Error("the dampening was silent — no log line explains why the escalation didn't happen")
	}
}
