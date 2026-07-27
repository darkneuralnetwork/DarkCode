// Package candidate ranks competing patches before any of them is kept.
//
// An agent asked for a fix produces one attempt and runs with it. Asking
// several times produces several attempts, and the interesting question is
// which to keep. Consensus already does this for *answers* by adjudicating
// claims against the knowledge graph; a patch admits a stronger test, because
// a patch can be run.
//
// So the ranking is deliberately lexicographic rather than a weighted blend:
//
//  1. does it apply at all
//  2. does the verifier pass
//  3. how much of the repository does it disturb
//
// A patch that passes the verifier beats every patch that does not, however
// elegant they look. Structure only orders patches that are already equal on
// evidence. Blending these into one number would let a tidy-looking patch
// outscore a working one, which is exactly the failure this is meant to
// prevent — and it is the same verifier-first rule the acceptance gate applies
// to plan nodes.
//
// Trials are injected rather than performed here, so the isolation strategy
// belongs to the caller and this package stays testable without a filesystem
// or a shell.
package candidate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/darkcode/memory"
)

// Patch is one proposed change: the full new content of each file it touches.
//
// Full content rather than a diff, because a diff has to be matched against
// the tree before it means anything, and two candidates that fail to apply for
// different reasons are indistinguishable at the point where they are being
// compared.
type Patch struct {
	ID string `json:"id"`
	// Source names what produced it — a model, a strategy, a repair pass — so
	// the reasons can say where the winner came from.
	Source string            `json:"source,omitempty"`
	Files  map[string]string `json:"files"`
}

// Paths returns the files this patch touches, sorted.
func (p Patch) Paths() []string {
	out := make([]string, 0, len(p.Files))
	for f := range p.Files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Trial is the outcome of actually trying a patch.
type Trial struct {
	// Applied reports whether the patch could be written to the tree.
	Applied bool `json:"applied"`
	// Verified reports whether the project's verifier passed afterwards. It is
	// only meaningful when Applied.
	Verified bool `json:"verified"`
	// Output is what the verifier said, kept for the losing candidates too:
	// why the others failed is usually more informative than why one passed.
	Output string `json:"output,omitempty"`
	// Err records a trial that could not be completed, as distinct from one
	// that completed and failed.
	Err string `json:"error,omitempty"`
}

// TrialFunc applies a patch in isolation, runs the verifier, and restores the
// tree. Implementations must leave the workspace as they found it: a candidate
// that contaminates the next one makes the whole comparison meaningless.
type TrialFunc func(ctx context.Context, p Patch) Trial

// Score is one candidate's standing.
type Score struct {
	Patch Patch `json:"patch"`
	Trial Trial `json:"trial"`
	// Blast is the share of indexed files that depend on what this patch
	// touches, in [0,1]. Lower is safer.
	Blast float64 `json:"blast"`
	// Violations counts conventions the touched files break.
	Violations int `json:"violations"`
	// Churn is the total size of the new content, as a crude proxy for how
	// much was rewritten.
	Churn int `json:"churn"`
	// Tier is 0 verified, 1 applied but unverified, 2 did not apply. Ordering
	// is by tier first, always.
	Tier    int      `json:"tier"`
	Reasons []string `json:"reasons"`
}

// Ranker scores candidates against a repository.
type Ranker struct {
	// KG supplies the structural signals. Nil disables them, leaving the
	// verifier as the only discriminator — degraded but still correct.
	KG *memory.KnowledgeGraph
	// Patterns are the conventions to hold candidates to, typically from the
	// pattern library so rules learned elsewhere apply here too.
	Patterns []memory.Pattern
	// Trial runs one candidate. Required: without it nothing can be ranked on
	// evidence and the whole exercise reduces to guessing.
	Trial TrialFunc
}

// tiers, in the order they are preferred.
const (
	TierVerified   = 0
	TierUnverified = 1
	TierBroken     = 2
)

// Rank tries every candidate and returns them best-first.
//
// Candidates are tried in the order given and the ordering of the result is
// total, so the same input produces the same winner every time.
func (r *Ranker) Rank(ctx context.Context, patches []Patch) ([]Score, error) {
	if r.Trial == nil {
		return nil, fmt.Errorf("candidate: no trial function, so nothing can be verified")
	}
	if len(patches) == 0 {
		return nil, nil
	}

	scores := make([]Score, 0, len(patches))
	for _, p := range patches {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		scores = append(scores, r.score(ctx, p))
	}

	sort.SliceStable(scores, func(i, j int) bool { return less(scores[i], scores[j]) })
	return scores, nil
}

// score runs one candidate and gathers its structural cost.
func (r *Ranker) score(ctx context.Context, p Patch) Score {
	s := Score{Patch: p, Trial: r.Trial(ctx, p)}

	switch {
	case !s.Trial.Applied:
		s.Tier = TierBroken
		s.Reasons = append(s.Reasons, "does not apply"+detail(s.Trial.Err))
	case s.Trial.Verified:
		s.Tier = TierVerified
		s.Reasons = append(s.Reasons, "the verifier passed")
	default:
		s.Tier = TierUnverified
		s.Reasons = append(s.Reasons, "applies, but the verifier failed"+detail(firstLine(s.Trial.Output)))
	}

	for _, content := range p.Files {
		s.Churn += len(content)
	}

	// Structural cost describes the region the patch touches, computed from
	// the graph as it stands. It is not a prediction of the graph afterwards —
	// that would need a re-index per candidate, and the daemon already reports
	// the after state once something is actually kept.
	if r.KG != nil && len(p.Files) > 0 {
		imp := r.KG.BlastRadius(p.Paths(), 2)
		s.Blast = imp.Severity
		if len(imp.Affected) > 0 {
			s.Reasons = append(s.Reasons, fmt.Sprintf("reaches %d other file(s)", len(imp.Affected)))
		}
		if v := r.violationsIn(p); v > 0 {
			s.Violations = v
			s.Reasons = append(s.Reasons, fmt.Sprintf("breaks %d convention(s)", v))
		}
	}
	return s
}

// violationsIn counts convention breaches among the files this patch touches,
// so a candidate is not penalised for breaches that were already there.
func (r *Ranker) violationsIn(p Patch) int {
	if r.KG == nil || len(r.Patterns) == 0 {
		return 0
	}
	touched := map[string]bool{}
	for _, f := range p.Paths() {
		touched[f] = true
	}
	var n int
	for _, v := range r.KG.CheckPatterns(r.Patterns) {
		if touched[v.File] {
			n++
		}
	}
	return n
}

// less orders two scores, best first.
//
// Tier dominates. Within a tier the cheaper patch wins: less reach into the
// repository, then fewer broken conventions, then less rewritten. The final
// comparison is on id so the order is total and two runs cannot disagree.
func less(a, b Score) bool {
	if a.Tier != b.Tier {
		return a.Tier < b.Tier
	}
	if a.Blast != b.Blast {
		return a.Blast < b.Blast
	}
	if a.Violations != b.Violations {
		return a.Violations < b.Violations
	}
	if a.Churn != b.Churn {
		return a.Churn < b.Churn
	}
	return a.Patch.ID < b.Patch.ID
}

// Best returns the winning candidate, and whether it is one worth keeping.
//
// The second return is false when the best available candidate did not pass
// the verifier. Callers must treat that as "no result": presenting the least
// bad of several failures as an answer is how an agent talks itself into
// shipping something broken.
func Best(scores []Score) (Score, bool) {
	if len(scores) == 0 {
		return Score{}, false
	}
	return scores[0], scores[0].Tier == TierVerified
}

// Format renders the ranking for a human or a model.
func Format(scores []Score) string {
	if len(scores) == 0 {
		return "no candidates"
	}
	var b strings.Builder
	winner, ok := Best(scores)
	if ok {
		fmt.Fprintf(&b, "%s wins: %s\n\n", label(winner.Patch), strings.Join(winner.Reasons, ", "))
	} else {
		fmt.Fprintf(&b, "no candidate passed the verifier — none of these should be kept\n\n")
	}
	for i, s := range scores {
		fmt.Fprintf(&b, "%d. %-22s %-11s blast %.0f%%  %d file(s)  %s\n",
			i+1, label(s.Patch), tierName(s.Tier), s.Blast*100, len(s.Patch.Files),
			strings.Join(s.Reasons, "; "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func label(p Patch) string {
	if p.Source != "" {
		return p.ID + " (" + p.Source + ")"
	}
	return p.ID
}

func tierName(t int) string {
	switch t {
	case TierVerified:
		return "verified"
	case TierUnverified:
		return "unverified"
	default:
		return "broken"
	}
}

func detail(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
