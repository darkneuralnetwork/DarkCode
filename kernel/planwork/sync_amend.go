package planwork

// sync_amend.go — the pre-turn plan amend gate, as one thing every surface
// does the same way.
//
// Before this, only the web surface amended the plan+workflow synchronously
// before a turn executed; the console relied solely on the post-turn async
// refresh (refresh.go), which runs after a turn already used whatever plan
// was left over from the PREVIOUS turn. Asking the same question from a
// terminal instead of a browser meant execution ran one turn behind, and the
// console fed the kernel context.md (conversational memory) instead of
// plan.md (the actual implementation plan) as a result of never having its
// own amend path to keep in sync.

import (
	"context"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/orchestrator"
)

// shortContinuationMaxLen bounds what counts as a "bare continuation"
// ("continue", "yes", "go on") for NeedsAmend — long enough to cover short
// acknowledgements, short enough that a real (if terse) instruction still
// triggers a real amend.
const shortContinuationMaxLen = 30

// syncAmendTimeout bounds the pre-turn rewrite. Unlike refresh.go's
// refreshTimeout (which runs after the user already has an answer), this
// runs BEFORE execution, so it must stay tight enough that an active project
// doesn't make every turn feel slow.
const syncAmendTimeout = 15 * time.Second

// NeedsAmend reports whether query should trigger a synchronous plan+workflow
// rewrite before Execute runs. It skips the amend only for a bare
// continuation ("continue"/"yes") after a real prior turn, using the same
// signal as the clarification gate; anything else is a plausible new
// instruction.
func NeedsAmend(query string, stm []core.Message, skipReadOnly bool) bool {
	trimmed := strings.TrimSpace(query)
	if orchestrator.HasActiveConversation(stm) && len(trimmed) < shortContinuationMaxLen {
		return false
	}
	// A read-only / question turn ("what does X do?", "explain the plan")
	// can't change the plan, so amending it is 2 wasted cloud calls. Skip
	// when SkipAuxForReadOnly is on.
	if skipReadOnly && orchestrator.QueryIsInformational(query) {
		return false
	}
	return true
}

// AmendSync synchronously rewrites projID's plan+workflow for a new
// instruction before the kernel executes it, so execution runs against a
// fresh plan instead of whatever the previous turn's async refresh left
// behind. Bounded by a tight sub-timeout; on timeout/error the old
// plan/workflow are returned unchanged (fail-open, same contract as Amend).
//
// One implementation, shared by every surface — this used to be a second
// copy of the same prompt with its own model-selection and metering logic;
// see this package's doc comment for what the two copies used to disagree
// about.
func AmendSync(ctx context.Context, store Store, router Router, notifier Notifier, projID, query, oldPlan, oldWorkflow string) (plan, workflow string) {
	ctx, cancel := context.WithTimeout(ctx, syncAmendTimeout)
	defer cancel()

	// Prefer the aux router's local-first pick (same contract Refresh uses);
	// fall back to the planner-tier client if no aux model is available. If
	// both fail, client stays nil and Amend returns oldPlan/oldWorkflow
	// unchanged (fail-open).
	client, model, ok := router.RouteAux("plan_amend", 0)
	if !ok || client == nil {
		client, model, _ = router.PlannerClient()
	}

	plan, workflow = Amend(ctx, client, model, query, oldPlan, oldWorkflow)
	// Amend already applies InjectNodeStatus internally before returning
	// plan; re-applying here is a no-op (statuses haven't changed since) but
	// preserved to match the original behavior exactly during this move.
	plan = InjectNodeStatus(plan, workflow)

	if projID != "" && store != nil {
		_ = store.SetPlan(projID, plan)
		_ = store.SetWorkflow(projID, workflow)
		if notifier != nil {
			notifier.EmitPlanUpdated(projID, plan)
			notifier.EmitWorkflowUpdated(projID, workflow)
		}
	}
	return plan, workflow
}
