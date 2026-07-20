package orchestrator

// plan_gate.go — the interactive plan approval gate.
//
// When a plan is produced and approval is required (cfg.PlanApproval:
// "always", or "auto" = deep-depth plans only), Execute returns the rendered
// plan preview instead of executing, and holds the graph as pending. The
// user's NEXT message is the decision:
//   - approve  → execute the stored graph
//   - reject   → discard it
//   - anything else → treated as revision feedback: the primary model
//     revises the plan (stable task IDs, completed work preserved) and the
//     new preview is returned, still pending.
//
// The gate is purely conversational — it works identically in the GUI chat
// and the CLI with no UI changes, and it is checked BEFORE the cognition
// cascade so a cached answer can never swallow an "approve".

import (
	"context"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/plan"
)

// pendingPlanTTL bounds how long a proposal stays actionable. After it, the
// next message is treated as a brand-new request, not stale plan feedback.
const pendingPlanTTL = 30 * time.Minute

// pendingPlanState is a proposal awaiting the user's decision.
type pendingPlanState struct {
	graph   *plan.Graph
	created time.Time
}

type planDecision int

const (
	planFeedback planDecision = iota
	planApprove
	planReject
)

// classifyPlanDecision interprets the user's reply to a pending plan.
// Only short, unqualified messages count as approve/reject — anything with
// substance ("yes but add tests", "no, use python instead") is feedback so
// the user's actual intent reaches the reviser.
func classifyPlanDecision(msg string) planDecision {
	norm := strings.ToLower(strings.TrimSpace(msg))
	norm = strings.Trim(norm, " .!?")
	if norm == "" || len(norm) > 60 {
		return planFeedback
	}
	for _, q := range []string{" but ", " except", " however", " instead", " also ", " add ", " remove ", " change", " modify", " use ", " make ", " should"} {
		if strings.Contains(norm, q) {
			return planFeedback
		}
	}

	approvals := []string{
		"approve", "approved", "yes", "y", "yeah", "yep", "ok", "okay", "k",
		"go", "go ahead", "proceed", "execute", "run", "run it", "do it",
		"start", "begin", "confirm", "confirmed", "lgtm", "looks good",
		"looks good to me", "sounds good", "ship it", "accept", "accepted",
		"green light", "👍",
	}
	rejects := []string{
		"reject", "rejected", "no", "n", "nope", "cancel", "cancelled",
		"canceled", "discard", "stop", "abort", "drop it", "no thanks",
		"forget it", "never mind", "nevermind", "skip", "don't", "dont",
		"decline", "don't do it", "dont do it",
	}
	first := norm
	if i := strings.IndexAny(norm, " ,;:"); i > 0 {
		first = norm[:i]
	}
	for _, a := range approvals {
		if norm == a || first == a || strings.HasPrefix(norm, a+" please") {
			return planApprove
		}
	}
	for _, r := range rejects {
		if norm == r || first == r {
			return planReject
		}
	}
	return planFeedback
}

// planApprovalRequired resolves the configured approval policy for a plan of
// the given depth. "auto" gates only deep plans: light plans are cheap and
// low-risk, so pausing for approval would cost more user time than it saves.
func planApprovalRequired(cfg string, depth PlanDepth) bool {
	switch strings.ToLower(strings.TrimSpace(cfg)) {
	case "always":
		return true
	case "auto":
		return depth == PlanDepthDeep
	default: // "never", "off", "" (tests / embedded uses)
		return false
	}
}

// setPendingPlan stores a proposal awaiting the user's decision.
func (k *Kernel) setPendingPlan(g *plan.Graph) {
	k.mu.Lock()
	k.pendingPlan = &pendingPlanState{graph: g, created: time.Now()}
	k.mu.Unlock()
}

// PlanAwaitingApproval reports whether a plan proposal is currently pending.
// The server uses this to skip build-completeness auto-continue on proposal
// turns (a proposal produced no artifacts — "completing" it would spin).
func (k *Kernel) PlanAwaitingApproval() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.pendingPlan != nil && time.Since(k.pendingPlan.created) <= pendingPlanTTL
}

// PlannerClient exposes the router's primary/reasoning client — wrapped with
// retry + rate limiting — for the server's auxiliary LLM calls (auto-mode
// classifier, project plan/workflow seeding). Those calls previously built a
// bare unwrapped client, so a single transient 429 silently disabled auto
// mode-detection and left project blueprints as skeletons.
func (k *Kernel) PlannerClient() (core.LLMClient, string, error) {
	return k.router.PlannerRoute()
}

// PendingPlanGraph returns the plan proposal currently awaiting approval,
// if any. The server renders it into the active project's Blueprint tab so
// the user can review the plan there as well as in chat.
func (k *Kernel) PendingPlanGraph() (*plan.Graph, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pendingPlan == nil || time.Since(k.pendingPlan.created) > pendingPlanTTL {
		return nil, false
	}
	return k.pendingPlan.graph, true
}

// ConsumeApprovedPlan returns the most recently executed plan graph (with
// final node statuses) and clears it. The server persists it to the active
// project's graph.json.
func (k *Kernel) ConsumeApprovedPlan() (*plan.Graph, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	g := k.lastRunPlan
	k.lastRunPlan = nil
	return g, g != nil
}

// handlePendingPlan processes the user's reply when a plan is pending.
// handled=false means no (fresh) pending plan exists and Execute should
// treat the message normally.
func (k *Kernel) handlePendingPlan(ctx context.Context, msg string) (string, bool, error) {
	k.mu.Lock()
	p := k.pendingPlan
	if p != nil && time.Since(p.created) > pendingPlanTTL {
		k.pendingPlan = nil
		p = nil
	}
	k.mu.Unlock()
	if p == nil {
		return "", false, nil
	}

	switch classifyPlanDecision(msg) {
	case planApprove:
		k.mu.Lock()
		k.pendingPlan = nil
		k.mu.Unlock()
		k.log("plan", "Plan approved by user — executing graph")
		recallBlock := k.getRecallBlock(p.graph.Goal)
		out, err := k.executePlannedGraph(ctx, p.graph, recallBlock)
		return out, true, err

	case planReject:
		k.mu.Lock()
		k.pendingPlan = nil
		k.mu.Unlock()
		k.log("plan", "Plan rejected by user — discarded")
		resp := "Plan discarded — nothing was executed. Tell me how you'd like to proceed instead."
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: resp})
		if k.emitter != nil {
			k.emitter.EmitTaskUpdate("planner", "rejected", "Plan discarded by user")
			k.emitter.EmitFinalOutput(resp)
		}
		return resp, true, nil

	default: // revision feedback
		k.log("plan", "Plan feedback received — revising")
		revised, err := k.revisePlan(ctx, p.graph, msg)
		if err != nil {
			resp := "I couldn't revise the plan (" + err.Error() + "). The previous proposal still stands — reply approve, reject, or rephrase your changes."
			k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: resp})
			if k.emitter != nil {
				k.emitter.EmitFinalOutput(resp)
			}
			return resp, true, nil
		}
		k.setPendingPlan(revised)
		preview := plan.Preview(revised)
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: preview})
		if k.emitter != nil {
			k.emitter.EmitTaskUpdate("planner", "awaiting-approval",
				"Revised plan awaiting approval")
			k.emitter.EmitFinalOutput(preview)
		}
		return preview, true, nil
	}
}
