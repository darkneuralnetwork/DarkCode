package orchestrator

// debate.go — letting two models answer each other, once, when nothing else can
// settle it.
//
// The naive version of this — N models, R rounds of argument, vote at the end —
// is a worse build than it looks, and the research is unusually consistent
// about why. Accuracy plateaus at two or three rounds and two to four agents,
// and debate frequently fails to beat plain self-consistency at equal token
// cost. Unstructured rounds then lose accuracy to problem drift: agents wander
// off the original question and the degradation compounds per round. The
// published mitigations are a judge or abort, and an external anchor.
//
// More importantly, intrinsic self-critique does not reliably improve
// reasoning, while externally grounded feedback does. This codebase already has
// the grounded version of the same idea in two places: repair.go feeds a failing
// command's real output back into the loop, and consensus.go checks each
// candidate's claims against the knowledge graph. For anything machine-checkable,
// running the check beats any amount of model conversation and costs one call
// rather than N×R.
//
// So debate is not a mode here. It is the fallback for the one case the
// grounded paths cannot reach: the models genuinely disagree AND the graph has
// nothing to check them against. That branch previously just returned the
// synthesis and moved on.
//
// The gate is not only elegance. On a free tier metered at twenty requests a
// day, an unconditional three-model three-round debate is a nine-times
// multiplier — two questions and the day is gone.

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
)

// debateExcerptBudget bounds how much of each position is quoted into the
// critique prompt. A critique of a wall of text becomes a summary of it.
const debateExcerptBudget = 1800

// SetDebate turns conflict-triggered debate on or off at runtime.
func (k *Kernel) SetDebate(on bool) {
	k.mu.Lock()
	k.debateOn = on
	k.mu.Unlock()
}

// debateEnabled reports whether an exchange may run.
func (k *Kernel) debateEnabled() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.debateOn
}

// debateOutcome is what one exchange produced.
type debateOutcome struct {
	Ran        bool
	Rounds     int
	Resolved   string // the synthesised answer after the exchange
	Transcript string
}

// resolveByDebate runs ONE round of mutual critique between the two most
// divergent positions, then asks the primary to settle it.
//
// goal is re-pinned in every critique prompt. That is the published fix for
// problem drift and the reason this is capped at one round rather than trusted
// to stay on topic.
func (k *Kernel) resolveByDebate(ctx context.Context, goal string, consensus *core.ConsensusResult) debateOutcome {
	var out debateOutcome
	if !k.debateEnabled() || consensus == nil {
		return out
	}

	positions := usablePositions(consensus)
	if len(positions) < 2 {
		return out // nothing to disagree with
	}
	a, b := positions[0], positions[1]

	client, model, err := k.router.Route(core.ModelTierReasoning, 8, goal)
	if err != nil || client == nil {
		return out
	}

	k.log("debate", fmt.Sprintf("Models disagree and the graph cannot settle it — one round between %s and %s", a.Model, b.Model))
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("debate", "running",
			fmt.Sprintf("%s and %s disagree; exchanging critiques once", a.Model, b.Model))
	}

	// Each side reads the other, anchored on the original question.
	critA := k.critique(ctx, client, model, goal, a, b)
	critB := k.critique(ctx, client, model, goal, b, a)

	// Record the exchange on the bus. It has carried MsgCritiqueRequest since
	// it was written and never sent one; this is the first traffic on it, and
	// it makes the exchange inspectable rather than invisible.
	k.publishCritique(a.Model, b.Model, goal, critA)
	k.publishCritique(b.Model, a.Model, goal, critB)

	var t strings.Builder
	fmt.Fprintf(&t, "### %s critiquing %s\n%s\n\n### %s critiquing %s\n%s\n",
		a.Model, b.Model, critA, b.Model, a.Model, critB)
	out.Transcript = t.String()

	resolved := k.settleDebate(ctx, client, model, goal, a, b, critA, critB)
	if strings.TrimSpace(resolved) == "" {
		return out // the exchange happened but produced nothing usable
	}

	out.Ran, out.Rounds, out.Resolved = true, 1, resolved
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("debate", "resolved", "one round, then synthesised")
	}
	return out
}

// critique asks one position to find the specific flaw in the other, with the
// original question re-pinned so the exchange cannot drift into a topic neither
// model was asked about.
func (k *Kernel) critique(ctx context.Context, client core.LLMClient, model, goal string, from, at core.ModelContribution) string {
	temp := 0.2
	maxTok := 350
	req := &core.CompletionRequest{
		Model: model,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "You are reviewing a disagreement between two answers to one question. " +
				"Name the specific point where the other answer is wrong or unsupported, and say what would settle it. " +
				"Do not restate your own answer. Do not broaden the question. If the other answer is right, say so plainly."},
			{Role: core.RoleUser, Content: fmt.Sprintf(
				"THE QUESTION (do not drift from this):\n%s\n\nYOUR POSITION:\n%s\n\nTHE OTHER POSITION:\n%s",
				goal,
				strutil.Truncate(from.Output, debateExcerptBudget),
				strutil.Truncate(at.Output, debateExcerptBudget))},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}
	resp, err := client.ChatCompletion(ctx, req)
	if err != nil || len(resp.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content)
}

// settleDebate asks for a final answer given both positions and both critiques.
// This is the judge the drift research calls for: the exchange ends here rather
// than looping.
func (k *Kernel) settleDebate(ctx context.Context, client core.LLMClient, model, goal string,
	a, b core.ModelContribution, critA, critB string) string {
	temp := 0.3
	maxTok := 1200
	req := &core.CompletionRequest{
		Model: model,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "Two answers disagreed and have now critiqued each other. " +
				"Give the single best answer to the original question. Prefer the position whose critique went " +
				"unanswered. Where the disagreement is genuinely unresolved, say so and state what would settle " +
				"it rather than picking arbitrarily."},
			{Role: core.RoleUser, Content: fmt.Sprintf(
				"QUESTION:\n%s\n\nPOSITION A (%s):\n%s\n\nPOSITION B (%s):\n%s\n\n"+
					"A'S CRITIQUE OF B:\n%s\n\nB'S CRITIQUE OF A:\n%s",
				goal, a.Model, strutil.Truncate(a.Output, debateExcerptBudget),
				b.Model, strutil.Truncate(b.Output, debateExcerptBudget), critA, critB)},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}
	resp, err := client.ChatCompletion(ctx, req)
	if err != nil || len(resp.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content)
}

// publishCritique records one side of the exchange on the agent bus.
func (k *Kernel) publishCritique(from, to, goal, body string) {
	if k.agentBus == nil || body == "" {
		return
	}
	k.agentBus.Send(core.AgentMessage{
		Kind:     core.MsgCritiqueRequest,
		Sender:   core.RoleCritic,
		Receiver: core.RoleCritic,
		Priority: core.MsgPriorityNormal,
		Task:     strutil.Truncate(goal, 120),
		Payload:  from + " → " + to + ": " + body,
	})
}

// usablePositions returns the contributions worth putting against each other:
// the ones that succeeded and actually said something, most divergent first.
func usablePositions(c *core.ConsensusResult) []core.ModelContribution {
	var out []core.ModelContribution
	for _, x := range c.Contributions {
		if x.Error == "" && strings.TrimSpace(x.Output) != "" {
			out = append(out, x)
		}
	}
	return out
}

// ApplyDebateOverride forces the exchange on for one request, returning a
// restore func to defer. This is what /debate does: the user has said the
// question is contested, so the conflict gate is bypassed rather than waited
// for.
func (k *Kernel) ApplyDebateOverride(on bool) func() {
	k.mu.Lock()
	prev := k.debateOn
	k.debateOn = on
	k.mu.Unlock()
	return func() {
		k.mu.Lock()
		k.debateOn = prev
		k.mu.Unlock()
	}
}
