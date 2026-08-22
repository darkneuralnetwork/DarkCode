package orchestrator

// reviewer.go — "this is correct; here is how it could be better".
//
// The system already has six opinion personas (critic, skeptic, verifier,
// analyst, creative, knowledge_booster), and a seventh opinion would add cost
// without accuracy — the research on self-critique is clear that intrinsic
// review does not reliably improve reasoning, while externally grounded
// feedback does. So a reviewer only earns its call under two conditions.
//
// It runs AFTER the acceptance gate has passed, never instead of it. Its job is
// not to decide whether the work is done — the tests decided that — but to say
// how the finished thing could be better. And it can never fail a run: a review
// that can turn a proven pass into a failure is a second gate wearing a
// different name, and the whole point of proving completion mechanically was to
// stop opinions deciding it.
//
// It is grounded in the knowledge graph rather than in vibes. "This contradicts
// a pattern the graph already records" is a claim a reader can check; "this
// could be cleaner" is not, and is what makes most automated review ignorable.
//
// Off by default. On a metered free tier an extra call on every successful run
// is a real cost for advice nobody asked for.

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/kernel/modelport"
	"github.com/darkcode/kernel/plan"
)

// reviewMaxTokens bounds the review. Advice that runs longer than the answer it
// is about does not get read.
const reviewMaxTokens = 400

// reviewGoalBudget caps how much of the original goal and output is quoted back
// into the review prompt.
const reviewGoalBudget = 2000

// SetReviewer turns post-acceptance review on or off at runtime.
func (k *Kernel) SetReviewer(on bool) {
	k.mu.Lock()
	k.reviewerOn = on
	k.mu.Unlock()
}

// reviewerEnabled reports whether review should run.
func (k *Kernel) reviewerEnabled() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reviewerOn
}

// reviewProvenWork returns improvement notes for work whose acceptance checks
// passed, or "" when review is off, the work was not proven, or the call fails.
//
// Every one of those is a silent no-op on purpose. A review is an optional
// extra; failing the task because the optional extra could not run would be
// exactly backwards.
func (k *Kernel) reviewProvenWork(ctx context.Context, goal, output string, g *plan.Graph) string {
	if !k.reviewerEnabled() || strings.TrimSpace(output) == "" {
		return ""
	}
	// Only proven work. Unproven work has a more urgent problem than style, and
	// reviewing it would bury the failure under suggestions.
	if g == nil || !graphProven(g) {
		return ""
	}

	grounding := k.reviewGrounding(goal)
	// PurposeReview is the critic tier, degrading to the auxiliary ladder —
	// which is what this asked for by hand, plus a fallback it did not have:
	// with no critic registered the review was simply skipped.
	ans, err := k.models.Complete(ctx, modelport.Ask{
		Purpose:    modelport.PurposeReview,
		Complexity: 5,
		Goal:       "review",
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: reviewerSystemPrompt},
			{Role: core.RoleUser, Content: fmt.Sprintf(
				"GOAL:\n%s\n\nWHAT WAS PRODUCED:\n%s\n%s\n\n"+
					"The acceptance checks already PASSED. Do not re-litigate whether it works. "+
					"Give at most three concrete improvements, or say there are none worth making.",
				strutil.Truncate(goal, reviewGoalBudget),
				strutil.Truncate(output, reviewGoalBudget), grounding)},
		},
		MaxTokens: reviewMaxTokens,
	})
	if err != nil {
		return "" // advice that could not be fetched is not a failure
	}
	notes := strings.TrimSpace(ans.Text)
	if notes == "" || isNoSuggestion(notes) {
		return ""
	}
	return "\n\n**Review** _(the checks passed; these are optional improvements)_\n" + notes
}

const reviewerSystemPrompt = "You review work that has ALREADY been verified to work. " +
	"You are not a gate and cannot reject anything. Comment only on things a reader could check: " +
	"structure, duplication, a name that misleads, a pattern the project already uses that this " +
	"contradicts. Do not comment on whether it functions — that is settled. " +
	"Do not pad. If there is nothing worth changing, say exactly: NO SUGGESTIONS."

// isNoSuggestion recognises the model declining to invent advice.
func isNoSuggestion(s string) bool {
	u := strings.ToUpper(s)
	return strings.HasPrefix(u, "NO SUGGESTIONS") || len(s) < 24
}

// reviewGrounding pulls patterns the knowledge graph already records for this
// topic, so the review can point at something concrete rather than at taste.
func (k *Kernel) reviewGrounding(goal string) string {
	if k.memory == nil {
		return ""
	}
	kg := k.memory.KG()
	if kg == nil {
		return ""
	}
	var facts []string
	for _, t := range []core.KGNodeType{core.KGNodeDecision, core.KGNodeFix} {
		for _, n := range kg.FindByType(t) {
			if n == nil || n.Confidence < 0.5 {
				continue
			}
			if !strings.Contains(strings.ToLower(goal), strings.ToLower(n.Label)) {
				continue
			}
			detail := n.Properties["detail"]
			if detail == "" {
				detail = n.Properties["resolution"]
			}
			if detail != "" {
				facts = append(facts, "- "+n.Label+": "+strutil.Truncate(detail, 200))
			}
			if len(facts) >= 5 {
				break
			}
		}
	}
	if len(facts) == 0 {
		return ""
	}
	return "\n\nWHAT THIS PROJECT HAS ALREADY DECIDED (contradicting one of these is worth flagging):\n" +
		strings.Join(facts, "\n")
}

// graphProven reports whether the graph carries at least one passing check and
// no failing one.
func graphProven(g *plan.Graph) bool {
	passed := false
	for _, n := range g.Nodes {
		for _, p := range n.Proof {
			if p.Command == "" {
				continue
			}
			if !p.Passed {
				return false
			}
			passed = true
		}
	}
	return passed
}
