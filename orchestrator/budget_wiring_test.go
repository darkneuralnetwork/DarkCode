package orchestrator

import (
	"strings"
	"testing"

	"github.com/darkcode/metrics"
)

// TestCostGovernorReachesTheLoop guards the per-iteration spend check.
//
// Execute has always tested the cap at entry, which bounds how many REQUESTS
// start once the limit is reached. It says nothing about how much a single
// request spends, and one agentic run makes up to maxLoops model calls — times
// the fan-out on a consensus turn. A run that began just under the cap could
// therefore finish far past it, which is not what a spend cap means.
func TestCostGovernorReachesTheLoop(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	tracker := metrics.NewUsageTracker()
	gov := metrics.NewCostGovernor(tracker, metrics.BudgetLimits{
		PerSessionUSD: 0.01,
		Action:        metrics.BudgetActionBlock,
	})

	k.SetCostGovernor(gov)

	if k.agenticLoop == nil {
		t.Fatal("test kernel has no agentic loop to wire")
	}
	check := k.agenticLoop.BudgetCheck()
	if check == nil {
		t.Fatal("SetCostGovernor did not install a per-iteration check on the loop; " +
			"the cap would only be tested once per request")
	}

	// Under the limit: the run continues.
	if reason := check(); reason != "" {
		t.Errorf("with no spend recorded the loop should continue, got %q", reason)
	}

	// Past the limit: the run stops, and says which limit.
	tracker.Record(metrics.RequestRecord{Cost: 1.0})
	reason := check()
	if reason == "" {
		t.Fatal("spend past the cap did not stop the loop")
	}
	if !strings.Contains(reason, "limit") {
		t.Errorf("reason %q should name the limit that was reached", reason)
	}
}

// TestClearingTheGovernorClearsTheLoopCheck — a stale check would keep halting
// runs after the user removed their cap.
func TestClearingTheGovernorClearsTheLoopCheck(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	k.SetCostGovernor(metrics.NewCostGovernor(metrics.NewUsageTracker(), metrics.BudgetLimits{
		PerSessionUSD: 0.01, Action: metrics.BudgetActionBlock,
	}))
	if k.agenticLoop.BudgetCheck() == nil {
		t.Fatal("setup: expected a check to be installed")
	}

	k.SetCostGovernor(nil)
	if k.agenticLoop.BudgetCheck() != nil {
		t.Error("removing the cost governor left a stale budget check on the loop")
	}
}

// TestWarnModeDoesNotHaltTheRun — "warn" means tell me, not stop me. Execute
// already logs it; halting mid-run would turn a warning into a block.
func TestWarnModeDoesNotHaltTheRun(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	tracker := metrics.NewUsageTracker()
	k.SetCostGovernor(metrics.NewCostGovernor(tracker, metrics.BudgetLimits{
		PerSessionUSD: 0.01, Action: metrics.BudgetActionWarn,
	}))
	tracker.Record(metrics.RequestRecord{Cost: 1.0})

	if reason := k.agenticLoop.BudgetCheck()(); reason != "" {
		t.Errorf("warn mode halted the loop with %q; only block mode may stop a run", reason)
	}
}
