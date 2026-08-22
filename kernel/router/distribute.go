package router

// distribute.go — spreading independent work across the models that are
// registered, instead of sending all of it to one.
//
// Consensus and distribution look similar from a distance and pull in opposite
// directions. Consensus asks several models the SAME question and synthesises
// the answers: it buys quality and it costs more, because every model does the
// whole job. Distribution hands each model a DIFFERENT piece: it buys
// throughput and costs nothing extra, because the work was going to be done
// either way.
//
// Only the first existed. Route() resolves a tier to a single client, so when
// the DAG ran a wave of four independent tasks concurrently, all four went to
// the same model and queued behind one provider's rate limit — parallel in the
// executor, serial at the endpoint. On a metered tier that is also the fastest
// way to exhaust a quota.
//
// RouteWorker fixes the seam it actually sits at: which client a worker gets.
// With one model registered it behaves exactly like Route.

import (
	"fmt"

	"github.com/darkcode/infra/core"
)

// RoleSupporter marks a model registered to take a share of the primary's
// work rather than to critique it. Unlike the consensus roles (critic,
// skeptic, verifier …) a supporter is never asked to second-guess an answer —
// it is asked to produce part of one, which is why it is deliberately absent
// from RoleSelector.SelectRoles and is only ever picked up here.
const RoleSupporter = "supporter"

// RouteWorker returns the client for one worker in a concurrent wave. slot is
// the worker's index within that wave; workers with different slots get
// different models where the registration allows it.
//
// Selection order is deliberate. Slot 0 keeps whatever Route would have
// chosen, so a single-task wave and the first task of a large one behave
// identically to before and the primary stays on the critical path. Later
// slots prefer models registered as supporters, then any other eligible model,
// and only fall back to the primary when there is genuinely nothing else.
func (r *Router) RouteWorker(tier core.ModelTier, complexity int, taskDesc string, slot int) (core.LLMClient, string, error) {
	if slot <= 0 {
		return r.Route(tier, complexity, taskDesc)
	}

	pool := r.workerPool()
	if len(pool) == 0 {
		// Nothing to spread across. Falling back to Route rather than
		// failing keeps a single-model install on exactly its old path.
		return r.Route(tier, complexity, taskDesc)
	}

	// Index from (slot-1), not slot: slot 0 already returned above via Route,
	// so starting at slot would skip the first entry in the pool — which is
	// the supporter, the one model registered specifically for this.
	m := pool[(slot-1)%len(pool)]
	if m.Client == nil {
		return r.Route(tier, complexity, taskDesc)
	}
	if r.emitter != nil {
		r.emitter.EmitTaskUpdate("router", "distributing",
			fmt.Sprintf("worker %d → %s (%s)", slot, m.Name, roleOrGeneral(m.Role)))
	}
	return m.Client, m.Name, nil
}

// workerPool returns the models eligible to take a share of parallel work,
// supporters first so an explicitly-registered helper is used before a model
// whose real job is something else.
//
// Disabled models are skipped, and force-local restricts the pool to local
// models — the same hard guarantee Route makes, repeated here because a
// distribution path that quietly reached for a cloud model would be a way
// around it.
func (r *Router) workerPool() []RegisteredModel {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var supporters, others, primary []RegisteredModel
	for _, m := range r.allModels {
		if m.Client == nil || r.isModelDisabledLocked(m.Name) {
			continue
		}
		if r.forceLocal && !isLocalTier(m.Tier) {
			continue
		}
		switch {
		case m.Role == RoleSupporter:
			supporters = append(supporters, m)
		case m.IsPrimary:
			// Last resort. Slot 0 already has the primary, so handing it to
			// another slot as well is the one outcome distribution exists to
			// avoid — but it still beats refusing to run the task.
			primary = append(primary, m)
		default:
			others = append(others, m)
		}
	}
	return append(append(supporters, others...), primary...)
}

// WorkerPoolSize reports how many distinct models are available to share
// parallel work. The executor uses it to decide whether spreading a wave is
// worth doing at all.
func (r *Router) WorkerPoolSize() int { return len(r.workerPool()) }

func roleOrGeneral(role string) string {
	if role == "" {
		return "general"
	}
	return role
}
