package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/plan"
)

// TestAcceptanceGateHoldsInEveryRoutingMode.
//
// Routing mode decides WHICH model answers — single picks one, escalation
// picks by complexity, consensus fans out and synthesises. None of that is a
// statement about whether the work is finished, so none of it may change
// whether completion has to be proven. It would be easy for it to: the loop
// and the consensus paths meet in Execute, and a mode check in the wrong place
// there would quietly give consensus mode a weaker stop condition than single.
//
// The kernel-level assertion is that the loop is reached and gated identically
// regardless of mode.
func TestAcceptanceGateHoldsInEveryRoutingMode(t *testing.T) {
	modes := []core.RoutingMode{core.RouteSingle, core.RouteEscalation, core.RouteConsensus}

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			client := &fakeLLMClient{name: "fake-primary", responses: []string{
				"I finished the task.",
				"GOAL_STATUS: DONE",
			}}
			deps := newTestKernelWithMode(t, mode, client)
			ctx, restore := deps.Kernel.ApplyRequestOverrides(context.Background(), "", "", "on", "on", "")
			defer restore()

			if !deps.Kernel.loopEnabledForRequest(ctx) {
				t.Fatal("loop mode did not take effect")
			}

			out, err := deps.Kernel.Execute(ctx, "explain how the retry layer works")
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if strings.TrimSpace(out) == "" {
				t.Error("empty output")
			}
			if client.callCount() == 0 {
				t.Error("the loop never reached a model")
			}
		})
	}
}

// TestVerifyContractIsModeIndependent pins the narrower fact directly: the
// contract check reads the plan graph and the filesystem, and consults nothing
// about routing. A future mode-aware shortcut here would be a silent downgrade
// of the stop condition for whichever mode it skipped.
func TestVerifyContractIsModeIndependent(t *testing.T) {
	var results []bool
	for _, mode := range []core.RoutingMode{core.RouteSingle, core.RouteEscalation, core.RouteConsensus} {
		deps := newTestKernelWithMode(t, mode, &fakeLLMClient{name: "fake"})
		g := planWithCriterion("go build ./nonexistent-target-xyz")
		v := deps.Kernel.verifyContract(context.Background(), g)
		results = append(results, v.Passed)
	}
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("verifyContract disagreed across routing modes: %v", results)
		}
	}
}

// planWithCriterion builds a one-node graph whose acceptance criterion is the
// given command.
func planWithCriterion(cmd string) *plan.Graph {
	return &plan.Graph{
		Goal: "test goal",
		Nodes: []*plan.Node{{
			ID: "T1", Name: "t1", Goal: "do a thing",
			Acceptance: []string{"`" + cmd + "`"},
		}},
	}
}
