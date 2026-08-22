package planwork

// refresh.go — the post-turn plan refresh, as one thing every surface does.
//
// This ran in three different amounts depending on where the request came in:
// the web did it, the console did it differently, and the editor and headless
// paths did not do it at all. So asking the same question from a terminal
// instead of a browser left the project's plan untouched, and nobody could
// tell you why.

import (
	"context"
	"time"

	"github.com/darkcode/infra/core"
)

// refreshTimeout bounds the rewrite. It runs after the user already has their
// answer, so a slow model must not hold a goroutine open for the request's
// full lifetime — the console's copy inherited a 300s HTTP timeout this way.
const refreshTimeout = 60 * time.Second

// Store is the part of a project store the refresh needs.
type Store interface {
	GetPlan(projectID string) (string, error)
	GetWorkflow(projectID string) (string, error)
	SetPlan(projectID, plan string) error
	SetWorkflow(projectID, workflow string) error
}

// Router supplies the model for auxiliary work. Named for the kernel's
// RouteAux, which is the existing single decision point for calls that are not
// the user's request — it prefers a healthy local model, so a plan refresh does
// not spend cloud tokens.
type Router interface {
	RouteAux(task string, promptTokens int) (core.LLMClient, string, bool)
}

// Notifier receives plan/workflow updates so a connected UI redraws.
type Notifier interface {
	EmitPlanUpdated(projectID, content string)
	EmitWorkflowUpdated(projectID, content string)
}

// Refresher rewrites a project's plan and workflow after a turn.
type Refresher struct {
	store    Store
	router   Router
	notifier Notifier // optional
}

// NewRefresher returns nil when it has nothing to work with, so the caller can
// pass the result straight to uiport.WithPostTurn and get a no-op rather than a
// hook that fails on every turn.
func NewRefresher(store Store, router Router, notifier Notifier) *Refresher {
	if store == nil || router == nil {
		return nil
	}
	return &Refresher{store: store, router: router, notifier: notifier}
}

// Refresh rewrites the plan and workflow for projectID to reflect the turn.
// Every failure path is a silent return: the work already succeeded, and a
// stale plan is a better outcome than an error the user cannot act on.
func (r *Refresher) Refresh(ctx context.Context, projectID, query, output string) {
	if r == nil || projectID == "" || query == "" {
		return
	}

	client, model, ok := r.router.RouteAux("plan_amend", 0)
	if !ok || client == nil {
		return
	}

	// Detached from the request context on purpose: the caller's context is
	// cancelled as soon as the response is written, which would cancel this
	// before it started.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()

	oldPlan, err := r.store.GetPlan(projectID)
	if err != nil {
		return
	}
	oldWorkflow, err := r.store.GetWorkflow(projectID)
	if err != nil {
		return
	}

	amended := query
	if output != "" {
		amended += "\n\n(the agent has just completed this work: " + output + ")"
	}

	plan, workflow := Amend(ctx, client, model, amended, oldPlan, oldWorkflow)

	if plan != oldPlan {
		if err := r.store.SetPlan(projectID, plan); err == nil && r.notifier != nil {
			r.notifier.EmitPlanUpdated(projectID, plan)
		}
	}
	if workflow != oldWorkflow {
		if err := r.store.SetWorkflow(projectID, workflow); err == nil && r.notifier != nil {
			r.notifier.EmitWorkflowUpdated(projectID, workflow)
		}
	}
}
