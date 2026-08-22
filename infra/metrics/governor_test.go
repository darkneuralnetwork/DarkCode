package metrics

import (
	"testing"
	"time"
)

// spend records a cost-bearing request against a tracker (provider/model with
// no pricing-registry entry, so we set Cost directly).
func spend(tr *UsageTracker, usd float64) {
	tr.Record(RequestRecord{Provider: "test", Model: "m", Cost: usd, Success: true})
}

func TestGovernorDisabledWhenNoLimits(t *testing.T) {
	tr := NewUsageTracker()
	g := NewCostGovernor(tr, BudgetLimits{})
	spend(tr, 100.0)
	if d := g.Check(); !d.Allowed || d.Reason != "" {
		t.Errorf("unconfigured governor should always allow with no reason, got %+v", d)
	}
}

func TestGovernorSessionBlock(t *testing.T) {
	tr := NewUsageTracker()
	g := NewCostGovernor(tr, BudgetLimits{PerSessionUSD: 1.0, Action: BudgetActionBlock})

	spend(tr, 0.5)
	if d := g.Check(); !d.Allowed {
		t.Fatalf("under the cap should be allowed, got %+v", d)
	}
	spend(tr, 0.6) // total 1.1 >= 1.0
	d := g.Check()
	if d.Allowed {
		t.Fatalf("over the session cap in block mode should be refused, got %+v", d)
	}
	if d.Reason == "" {
		t.Error("a blocked decision must carry a reason")
	}
}

func TestGovernorSessionWarnProceeds(t *testing.T) {
	tr := NewUsageTracker()
	g := NewCostGovernor(tr, BudgetLimits{PerSessionUSD: 1.0, Action: BudgetActionWarn})

	spend(tr, 2.0) // over the cap
	d := g.Check()
	if !d.Allowed {
		t.Fatalf("warn mode must proceed even over the cap, got %+v", d)
	}
	if !d.Warn || d.Reason == "" {
		t.Errorf("warn mode over the cap should flag a warning with a reason, got %+v", d)
	}
}

func TestGovernorDayWindowRollsOver(t *testing.T) {
	tr := NewUsageTracker()
	g := NewCostGovernor(tr, BudgetLimits{PerDayUSD: 1.0, Action: BudgetActionBlock})

	// Pin "now" so we can advance the day deterministically.
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.Local)
	g.nowFn = func() time.Time { return base }
	g.dayAnchor = dayStart(base)
	g.costAtDayStart = 0

	spend(tr, 1.5) // over today's cap
	if d := g.Check(); d.Allowed {
		t.Fatalf("over the daily cap should be refused, got %+v", d)
	}

	// Advance to the next day: the day window resets, so the same cumulative
	// tracker total no longer counts against the new day's budget.
	g.nowFn = func() time.Time { return base.Add(24 * time.Hour) }
	if d := g.Check(); !d.Allowed {
		t.Fatalf("after the day rolls over, prior spend should not block the new day, got %+v", d)
	}

	// New spend within the new day accrues against the fresh window.
	spend(tr, 1.2)
	if d := g.Check(); d.Allowed {
		t.Fatalf("new-day spend over the cap should block again, got %+v", d)
	}
}

func TestParseBudgetAction(t *testing.T) {
	if ParseBudgetAction("block") != BudgetActionBlock {
		t.Error("'block' should parse to BudgetActionBlock")
	}
	for _, s := range []string{"", "warn", "nonsense"} {
		if ParseBudgetAction(s) != BudgetActionWarn {
			t.Errorf("ParseBudgetAction(%q) should default to warn", s)
		}
	}
}

func TestBudgetLimitsConfigured(t *testing.T) {
	if (BudgetLimits{}).Configured() {
		t.Error("empty limits should not be configured")
	}
	if !(BudgetLimits{PerSessionUSD: 1}).Configured() {
		t.Error("a session cap should count as configured")
	}
	if !(BudgetLimits{PerDayUSD: 1}).Configured() {
		t.Error("a day cap should count as configured")
	}
}
