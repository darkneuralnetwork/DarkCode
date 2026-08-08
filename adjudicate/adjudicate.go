// Package adjudicate settles which of several candidate answers is right.
//
// # WHY THIS IS ITS OWN COMPONENT
//
// Given N answers to one question, deciding between them is one job with two
// methods: check the claims against structural facts, and — only when checking
// is silent — let the two most divergent answers critique each other once.
// Splitting those across two managers would split one decision.
//
// It is not part of the model manager. Its PRIMARY method is knowledge-graph
// verification, so siting it there would drag memory into the model layer,
// which the layering forbids. Debate is the fallback, not the mechanism, and
// filing the whole thing under its fallback would put the cheap, correct path
// behind the expensive one.
//
// It is not part of the orchestrator either: the orchestrator is meant to
// coordinate rather than implement, and this was ~375 lines of it.
//
// So it sits beside both, takes candidates from the model manager and evidence
// from the data source manager, and neither of those learns about the other.
//
// # WHY DEBATE IS A FALLBACK AND CAPPED AT ONE ROUND
//
// The naive version — N models, R rounds, vote — is a worse build than it
// looks. Accuracy plateaus at two or three rounds and two to four agents, and
// debate frequently fails to beat plain self-consistency at equal token cost.
// Unstructured rounds then lose accuracy to problem drift: agents wander off
// the original question and the degradation compounds per round. The published
// mitigations are a judge or abort, and an external anchor — so the goal is
// re-pinned in every prompt and the exchange ends at one round.
//
// Intrinsic self-critique does not reliably improve reasoning; externally
// grounded feedback does. For anything machine-checkable, running the check
// beats any amount of model conversation and costs one call rather than N×R.
//
// The gate is not only elegance. On a free tier metered at twenty requests a
// day, an unconditional three-model three-round debate is a nine-times
// multiplier — two questions and the day is gone.
package adjudicate

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/datasource"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/modelport"
)

// excerptBudget bounds how much of each position is quoted into a critique
// prompt. A critique of a wall of text becomes a summary of it.
const excerptBudget = 1800

// Position is one model's contribution to a consensus round.
type Position = core.ModelContribution

// Evidence checks candidate answers against structural facts. Satisfied by
// *datasource.Manager, which reaches the knowledge graph.
//
// ok is false when there is no graph to check against — distinct from a graph
// that checked and found nothing, which is ok with zero Checked claims.
type Evidence interface {
	Adjudicate(candidates []string) (best int, supports []datasource.Support, ok bool)
}

// Recorder observes the exchange. Optional; satisfied by the agent bus so a
// debate is inspectable rather than invisible.
type Recorder interface {
	Critiqued(from, to, goal, body string)
}

// Result is the verdict.
type Result struct {
	// Answer is what the caller should use. Never empty when the consensus
	// carried a synthesis.
	Answer string
	// Method is how it was decided: "evidence", "debate", or "synthesis".
	Method string
	// Debated reports whether an exchange actually ran, and Transcript carries
	// it for display.
	Debated    bool
	Transcript string
	// Note is an optional warning to append to the answer — set when the graph
	// contradicted part of what is being returned anyway.
	Note string
}

// Adjudicator settles consensus rounds.
type Adjudicator struct {
	models *modelport.Manager
	ev     Evidence
	rec    Recorder
	// debateEnabled is consulted per call rather than stored, because it is a
	// runtime toggle. nil means never debate.
	debateEnabled func() bool
	log           func(string)
}

// New builds an adjudicator. Evidence and the model manager may be nil; the
// verdict then degrades to the synthesis rather than failing.
func New(models *modelport.Manager, ev Evidence, opts ...Option) *Adjudicator {
	a := &Adjudicator{models: models, ev: ev}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Option configures an Adjudicator.
type Option func(*Adjudicator)

// WithRecorder records each critique.
func WithRecorder(r Recorder) Option { return func(a *Adjudicator) { a.rec = r } }

// WithDebate gates the fallback exchange. Without it, debate never runs.
func WithDebate(enabled func() bool) Option {
	return func(a *Adjudicator) { a.debateEnabled = enabled }
}

// WithLog receives progress lines.
func WithLog(f func(string)) Option { return func(a *Adjudicator) { a.log = f } }

func (a *Adjudicator) logf(format string, args ...any) {
	if a.log != nil {
		a.log(fmt.Sprintf(format, args...))
	}
}

// Verdict decides which candidate answer to return.
//
// Evidence first, debate only when the evidence cannot distinguish them, and
// the synthesis on a tie — it saw every contribution, so it is the right
// default whenever the evidence does not actually separate the candidates.
func (a *Adjudicator) Verdict(ctx context.Context, goal string, c *core.ConsensusResult) Result {
	if c == nil {
		return Result{}
	}
	res := Result{Answer: c.Synthesized, Method: "synthesis"}
	if a.ev == nil {
		return res
	}

	candidates := []string{c.Synthesized}
	labels := []string{"synthesis"}
	for _, x := range c.Contributions {
		if strings.TrimSpace(x.Output) != "" {
			candidates = append(candidates, x.Output)
			labels = append(labels, x.Model)
		}
	}
	if len(candidates) < 2 {
		return res
	}

	best, supports, ok := a.ev.Adjudicate(candidates)
	if !ok || best < 0 || len(supports) == 0 || supports[0].Checked == 0 {
		// Nothing checkable. This is the one case where the models disagreeing
		// is all the information there is, and where letting them answer each
		// other is worth a call. Everything else is settled by evidence, which
		// is both cheaper and better.
		if c.Conflict {
			if d := a.debate(ctx, goal, c); d.Debated {
				a.logf("Debate settled a conflict the graph could not check")
				return d
			}
		}
		return res
	}

	res.Method = "evidence"
	if best != 0 && supports[best].Score() > supports[0].Score() {
		res.Answer = candidates[best]
		a.logf("Adjudicated on structure: %s (%d/%d claims verified) over the synthesis (%d/%d)",
			labels[best], supports[best].Verified, supports[best].Checked,
			supports[0].Verified, supports[0].Checked)
	}
	// Report what the graph refuted in the answer being returned, so a
	// surviving error is visible rather than silently authoritative.
	if wrong := supports[best].Contradicted(); len(wrong) > 0 && best == 0 {
		var lines []string
		for _, w := range wrong {
			lines = append(lines, "- "+w.Detail)
		}
		res.Note = "\n\n_⚠ The code graph contradicts part of this answer:_\n" + strings.Join(lines, "\n")
		res.Answer += res.Note
	}
	return res
}

// debate runs ONE round of mutual critique between the two most divergent
// positions, then asks for a settlement.
//
// goal is re-pinned in every prompt. That is the published fix for problem
// drift and the reason this is capped at one round rather than trusted to stay
// on topic.
func (a *Adjudicator) debate(ctx context.Context, goal string, c *core.ConsensusResult) Result {
	var out Result
	if a.debateEnabled == nil || !a.debateEnabled() || a.models == nil {
		return out
	}
	positions := usablePositions(c)
	if len(positions) < 2 {
		return out // nothing to disagree with
	}
	p, q := positions[0], positions[1]

	a.logf("Models disagree and the graph cannot settle it — one round between %s and %s", p.Model, q.Model)

	critP := a.critique(ctx, goal, p, q)
	critQ := a.critique(ctx, goal, q, p)
	if a.rec != nil {
		a.rec.Critiqued(p.Model, q.Model, goal, critP)
		a.rec.Critiqued(q.Model, p.Model, goal, critQ)
	}

	var t strings.Builder
	fmt.Fprintf(&t, "### %s critiquing %s\n%s\n\n### %s critiquing %s\n%s\n",
		p.Model, q.Model, critP, q.Model, p.Model, critQ)

	resolved := a.settle(ctx, goal, p, q, critP, critQ)
	if strings.TrimSpace(resolved) == "" {
		return out // the exchange happened but produced nothing usable
	}
	return Result{Answer: resolved, Method: "debate", Debated: true, Transcript: t.String()}
}

// critique asks one position to find the specific flaw in the other, with the
// original question re-pinned so the exchange cannot drift into a topic neither
// model was asked about.
func (a *Adjudicator) critique(ctx context.Context, goal string, from, at Position) string {
	ans, err := a.models.Complete(ctx, modelport.Ask{
		Purpose:    modelport.PurposeAdjudicate,
		Complexity: 8,
		Goal:       goal,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "You are reviewing a disagreement between two answers to one question. " +
				"Name the specific point where the other answer is wrong or unsupported, and say what would settle it. " +
				"Do not restate your own answer. Do not broaden the question. If the other answer is right, say so plainly."},
			{Role: core.RoleUser, Content: fmt.Sprintf(
				"THE QUESTION (do not drift from this):\n%s\n\nYOUR POSITION:\n%s\n\nTHE OTHER POSITION:\n%s",
				goal,
				strutil.Truncate(from.Output, excerptBudget),
				strutil.Truncate(at.Output, excerptBudget))},
		},
		MaxTokens: 350,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ans.Text)
}

// settle asks for a final answer given both positions and both critiques. This
// is the judge the drift research calls for: the exchange ends here rather than
// looping.
func (a *Adjudicator) settle(ctx context.Context, goal string, p, q Position, critP, critQ string) string {
	ans, err := a.models.Complete(ctx, modelport.Ask{
		Purpose:    modelport.PurposeAdjudicate,
		Complexity: 8,
		Goal:       goal,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "Two answers disagreed and have now critiqued each other. " +
				"Give the single best answer to the original question. Prefer the position whose critique went " +
				"unanswered. Where the disagreement is genuinely unresolved, say so and state what would settle " +
				"it rather than picking arbitrarily."},
			{Role: core.RoleUser, Content: fmt.Sprintf(
				"QUESTION:\n%s\n\nPOSITION A (%s):\n%s\n\nPOSITION B (%s):\n%s\n\n"+
					"A'S CRITIQUE OF B:\n%s\n\nB'S CRITIQUE OF A:\n%s",
				goal, p.Model, strutil.Truncate(p.Output, excerptBudget),
				q.Model, strutil.Truncate(q.Output, excerptBudget), critP, critQ)},
		},
		MaxTokens: 1200,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ans.Text)
}

// usablePositions returns the contributions worth putting against each other:
// the ones that succeeded and actually said something.
func usablePositions(c *core.ConsensusResult) []Position {
	var out []Position
	for _, x := range c.Contributions {
		if x.Error == "" && strings.TrimSpace(x.Output) != "" {
			out = append(out, x)
		}
	}
	return out
}
