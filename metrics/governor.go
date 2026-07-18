package metrics

import (
	"fmt"
	"sync"
	"time"
)

// ============================================================================
// COST GOVERNOR — budget enforcement layered on top of UsageTracker.
//
// The UsageTracker already accumulates per-request cost (from the provider
// pricing registry; local/unknown models cost nothing). The governor reuses
// that accumulation rather than tracking spend a second time — it only adds
// the *policy*: configurable session/day spend caps and what to do when one
// is reached.
//
// Design choices worth stating:
//   - Budgets default to 0 (unlimited) → the governor is a no-op unless a
//     limit is explicitly configured, so it never changes existing behavior
//     by surprise.
//   - Only session and day budgets are enforced. A per-request budget would
//     require pre-flight cost estimation, which depends on token estimation
//     the codebase itself flags as approximate — enforcing a hard cap on a
//     guessed number would be worse than not enforcing it.
//   - Because local models cost 0, budgets only ever constrain cloud spend,
//     which is the intent ("never call an expensive cloud model if you don't
//     have to"). A block that stops cloud calls still leaves local models
//     usable if the caller degrades to them.
// ============================================================================

// BudgetAction is what the governor does when a spend cap is reached.
type BudgetAction string

const (
	// BudgetActionWarn logs/surfaces the overage but lets the request proceed.
	// This is the default: a cost governor should never silently block a
	// user's work unless they explicitly asked it to.
	BudgetActionWarn BudgetAction = "warn"
	// BudgetActionBlock refuses new requests once a cap is reached.
	BudgetActionBlock BudgetAction = "block"
)

// ParseBudgetAction maps a config string to a BudgetAction, defaulting to warn.
func ParseBudgetAction(s string) BudgetAction {
	if s == string(BudgetActionBlock) {
		return BudgetActionBlock
	}
	return BudgetActionWarn
}

// BudgetLimits configures spend caps (USD). 0 = unlimited for that dimension.
type BudgetLimits struct {
	PerSessionUSD float64
	PerDayUSD     float64
	Action        BudgetAction
}

// Configured reports whether any cap is set (otherwise the governor is inert).
func (b BudgetLimits) Configured() bool {
	return b.PerSessionUSD > 0 || b.PerDayUSD > 0
}

// CostGovernor enforces BudgetLimits against a UsageTracker's accumulated cost.
type CostGovernor struct {
	mu      sync.Mutex
	tracker *UsageTracker
	limits  BudgetLimits

	// Day-window bookkeeping: the tracker only knows session-cumulative cost,
	// so the governor derives day spend as (current total − total at the
	// start of the current local day), rolling the anchor at each day change.
	dayAnchor      time.Time
	costAtDayStart float64
	nowFn          func() time.Time // injectable for tests
}

// NewCostGovernor builds a governor over the given tracker (defaults to the
// process-wide Default tracker if nil).
func NewCostGovernor(tracker *UsageTracker, limits BudgetLimits) *CostGovernor {
	if tracker == nil {
		tracker = Default
	}
	g := &CostGovernor{tracker: tracker, limits: limits, nowFn: time.Now}
	g.dayAnchor = dayStart(g.nowFn())
	g.costAtDayStart = tracker.Snapshot().TotalCost
	return g
}

// SetLimits updates the caps at runtime (e.g. from a config hot-reload).
func (g *CostGovernor) SetLimits(limits BudgetLimits) {
	g.mu.Lock()
	g.limits = limits
	g.mu.Unlock()
}

// Decision is the result of a pre-request budget check.
type Decision struct {
	Allowed bool
	// Reason is non-empty when a cap has been reached (whether or not the
	// request is blocked), so callers can log/surface it in either mode.
	Reason string
	// Warn is true when a cap is reached but Action is warn (proceed anyway).
	Warn bool
}

// Check reports whether a new request may proceed under the current budget.
// It reads cumulative spend from the tracker (no separate accounting) and
// applies the configured action. When no cap is configured it always allows.
func (g *CostGovernor) Check() Decision {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.limits.Configured() {
		return Decision{Allowed: true}
	}

	total := g.tracker.Snapshot().TotalCost

	// Roll the day window if we've crossed a local-day boundary.
	if today := dayStart(g.nowFn()); today.After(g.dayAnchor) {
		g.dayAnchor = today
		g.costAtDayStart = total
	}

	var reason string
	if g.limits.PerSessionUSD > 0 && total >= g.limits.PerSessionUSD {
		reason = fmt.Sprintf("session spend $%.4f reached the $%.2f limit", total, g.limits.PerSessionUSD)
	} else if daySpend := total - g.costAtDayStart; g.limits.PerDayUSD > 0 && daySpend >= g.limits.PerDayUSD {
		reason = fmt.Sprintf("today's spend $%.4f reached the $%.2f daily limit", daySpend, g.limits.PerDayUSD)
	}

	if reason == "" {
		return Decision{Allowed: true}
	}
	if g.limits.Action == BudgetActionBlock {
		return Decision{Allowed: false, Reason: reason}
	}
	return Decision{Allowed: true, Reason: reason, Warn: true}
}

// dayStart returns local midnight for t.
func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
