package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/router"
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

	out, res, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rewrite the parser", "", 5)
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

	out, _, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rename a field", "", 5)
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

	out, res, ok := deps.Kernel.escalateStuckLoop(context.Background(), "rewrite the parser", "", 5)
	if ok {
		t.Error("escalation claimed success with an unreachable planner")
	}
	if out != "" || res != nil {
		t.Errorf("failed escalation produced output: %q", out)
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
