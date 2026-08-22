// Package selfheal turns structural findings into changes that have been
// proven to work before anyone is asked to look at them.
//
// The knowledge graph already reports what is wrong: dead symbols, untested
// hotspots, files breaking a convention the rest of the repository keeps.
// Reporting is where that has stopped. The gap between "here are eleven
// findings" and "here are three fixes, each with a green test run" is the
// whole distance between a linter and something worth automating.
//
// The design is built around one rule: nothing leaves this package that has
// not passed the project's own verifier. Not "the model said it was fine", not
// "it looked plausible" — the repository's test command exited zero with the
// change applied. Candidate patches are ranked by that rule (see the candidate
// package), and a finding whose best candidate failed produces no output at
// all rather than a hopeful suggestion.
//
// The second rule is that this package never publishes. It prepares a branch
// and a commit locally and stops. Pushing and opening a pull request are
// outward-facing acts that a person authorises, and an agent that can open
// pull requests unattended is a different product with a different risk
// profile from one that leaves them staged on a branch.
package selfheal

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/candidate"
	"github.com/darkcode/memory"
)

// Issue is something worth fixing, found structurally.
type Issue struct {
	Kind    string   `json:"kind"`
	Subject string   `json:"subject"`
	Detail  string   `json:"detail"`
	Files   []string `json:"files"`
	// Weight orders the work; higher is more worth doing.
	Weight float64 `json:"weight"`
}

// GenerateFunc proposes candidate patches for one issue.
//
// Injected rather than implemented here: producing a fix needs a model, and
// binding this package to one would make the proof gate — the part that
// matters — untestable without a live provider.
type GenerateFunc func(ctx context.Context, issue Issue) ([]candidate.Patch, error)

// Fix is a change that passed the verifier, ready for a person to review.
type Fix struct {
	Issue  Issue           `json:"issue"`
	Patch  candidate.Patch `json:"patch"`
	Score  candidate.Score `json:"score"`
	Branch string          `json:"branch"`
	Title  string          `json:"title"`
	Body   string          `json:"body"`
	// Rejected records the candidates that were tried and failed, so a review
	// can see what else was attempted rather than only the survivor.
	Rejected []candidate.Score `json:"rejected,omitempty"`
}

// Healer finds issues, proposes fixes, and proves them.
type Healer struct {
	Workspace string
	KG        *memory.KnowledgeGraph
	Patterns  *memory.PatternLibrary
	// Verify is the command whose exit status decides whether a fix works.
	Verify string
	// Generate produces candidates. Without it nothing can be proposed.
	Generate GenerateFunc
	// MaxFixes bounds one run. Each fix costs several verifier runs, and a
	// review of thirty automated changes does not happen.
	MaxFixes int
}

// defaultMaxFixes keeps a run reviewable. The limit is about the person at the
// other end, not about machine time.
const defaultMaxFixes = 3

// FindIssues collects what the graph says is wrong, worst first.
//
// Only issues a patch could plausibly address are included. An import cycle is
// real and serious but fixing one is an architectural decision, not an
// automatable edit, so it is reported by the daemon and left alone here.
func (h *Healer) FindIssues() []Issue {
	if h.KG == nil {
		return nil
	}
	var out []Issue

	for _, f := range h.KG.UntestedHotspots(20) {
		out = append(out, Issue{
			Kind: "untested-hotspot", Subject: f.Subject, Detail: f.Detail,
			Files: filesFromProvenance(f.Detail), Weight: f.Weight,
		})
	}

	var rules []memory.Pattern
	if h.Patterns != nil {
		rules = h.Patterns.All()
	}
	if len(rules) == 0 {
		rules = h.KG.MinePatterns(h.Workspace)
	}
	for _, v := range h.KG.CheckPatterns(rules) {
		// Only the test-companion rule suggests a mechanical fix: write the
		// missing test. A layering violation means moving code across a
		// boundary, which is a design change.
		if v.Pattern.Kind != "test-companion" {
			continue
		}
		out = append(out, Issue{
			Kind: "missing-test", Subject: v.File, Detail: v.Detail,
			Files: []string{v.File}, Weight: 0.5,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// filesFromProvenance pulls the "(path:line)" a finding carries.
func filesFromProvenance(detail string) []string {
	open := strings.LastIndex(detail, "(")
	close := strings.LastIndex(detail, ")")
	if open < 0 || close < open {
		return nil
	}
	ref := detail[open+1 : close]
	if path, _, ok := strings.Cut(ref, ":"); ok && path != "" {
		return []string{path}
	}
	if ref != "" {
		return []string{ref}
	}
	return nil
}

// Propose generates fixes for the given issues and keeps only the proven ones.
//
// A finding whose candidates all failed contributes nothing to the result. It
// is not reported as a weaker suggestion, because the point of the exercise is
// that everything coming out of it has been run.
func (h *Healer) Propose(ctx context.Context, issues []Issue) ([]Fix, error) {
	if h.Generate == nil {
		return nil, fmt.Errorf("selfheal: no generator, so no fix can be proposed")
	}
	verify := h.Verify
	if verify == "" {
		verify = candidate.DefaultVerify(h.Workspace)
	}
	if verify == "" {
		return nil, fmt.Errorf("selfheal: no verify command for %s, so no fix could be proven", h.Workspace)
	}

	limit := h.MaxFixes
	if limit <= 0 {
		limit = defaultMaxFixes
	}

	ranker := &candidate.Ranker{
		KG:    h.KG,
		Trial: candidate.FileTrial(h.Workspace, verify),
	}
	if h.Patterns != nil {
		ranker.Patterns = h.Patterns.All()
	}

	var fixes []Fix
	for _, issue := range issues {
		if len(fixes) >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return fixes, err
		}

		patches, err := h.Generate(ctx, issue)
		if err != nil || len(patches) == 0 {
			continue // a generator that could not help is not a failure of the run
		}
		scores, err := ranker.Rank(ctx, patches)
		if err != nil {
			return fixes, err
		}
		best, proven := candidate.Best(scores)
		if !proven {
			continue // the gate: unverified means nothing leaves here
		}

		fix := Fix{
			Issue: issue, Patch: best.Patch, Score: best,
			Branch: branchName(issue),
			Title:  title(issue),
		}
		if len(scores) > 1 {
			fix.Rejected = scores[1:]
		}
		fix.Body = body(fix, verify)
		fixes = append(fixes, fix)
	}
	return fixes, nil
}

// BranchFor is the branch a fix for this issue belongs on. Exported so a
// caller staging its own fix lands on the same name this package would pick.
func BranchFor(i Issue) string { return branchName(i) }

// branchName builds a predictable, collision-resistant branch name.
func branchName(i Issue) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == '/' || r == '.' || r == '_' || r == ' ':
			return '-'
		}
		return -1
	}, i.Subject)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "issue"
	}
	return fmt.Sprintf("selfheal/%s-%s", i.Kind, slug)
}

func title(i Issue) string {
	switch i.Kind {
	case "missing-test":
		return "add a test for " + i.Subject
	case "untested-hotspot":
		return "cover " + i.Subject + " with a test"
	}
	return "fix " + i.Subject
}

// body writes the PR description. It states what was verified and what was
// rejected, because a reviewer's first question about an automated change is
// what evidence there is, and their second is what else was tried.
func body(f Fix, verify string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", f.Issue.Detail)
	fmt.Fprintf(&b, "Verified with `%s`, which exited zero with this change applied.\n\n", verify)

	fmt.Fprintf(&b, "Files changed:\n")
	for _, p := range f.Patch.Paths() {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	if f.Score.Blast > 0 {
		fmt.Fprintf(&b, "\nStructural reach: %.0f%% of indexed files depend on what this touches.\n",
			f.Score.Blast*100)
	}
	if len(f.Rejected) > 0 {
		fmt.Fprintf(&b, "\nOther candidates tried and rejected:\n")
		for _, r := range f.Rejected {
			fmt.Fprintf(&b, "- %s: %s\n", r.Patch.ID, strings.Join(r.Reasons, "; "))
		}
	}
	b.WriteString("\nProposed automatically from a structural finding. " +
		"It has been run, not reviewed — the reading is still yours.\n")
	return b.String()
}

// Stage writes a fix to its own branch and commits it, leaving the working
// tree on the branch it started from.
//
// It stops there. Pushing and opening a pull request are outward-facing and
// belong to a person; this leaves the work somewhere they can look at it.
func (h *Healer) Stage(ctx context.Context, f Fix) error {
	start, err := h.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the current branch: %w", err)
	}
	start = strings.TrimSpace(start)
	if start == "" || start == "HEAD" {
		return fmt.Errorf("selfheal: not on a branch (detached HEAD), refusing to stage")
	}
	if dirty, err := h.git(ctx, "status", "--porcelain"); err == nil && strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("selfheal: the working tree has uncommitted changes; " +
			"staging a fix on top of them would mix the two")
	}

	if _, err := h.git(ctx, "checkout", "-b", f.Branch); err != nil {
		return fmt.Errorf("creating %s: %w", f.Branch, err)
	}
	// Whatever happens next, put the checkout back where it was.
	defer func() { _, _ = h.git(context.WithoutCancel(ctx), "checkout", start) }()

	undo, err := writeFiles(h.Workspace, f.Patch)
	if err != nil {
		undo()
		return fmt.Errorf("writing the fix: %w", err)
	}
	// Stage exactly the files the patch declares, so a stray file that was
	// already untracked cannot ride along in an automated commit.
	if _, err := h.git(ctx, append([]string{"add", "--"}, f.Patch.Paths()...)...); err != nil {
		undo()
		return fmt.Errorf("staging the fix: %w", err)
	}
	if _, err := h.git(ctx, "commit", "-m", f.Title, "-m", f.Body); err != nil {
		undo()
		return fmt.Errorf("committing the fix: %w", err)
	}
	return nil
}

// writeFiles applies a patch to disk, returning an undo for the failure path.
func writeFiles(workspace string, p candidate.Patch) (func(), error) {
	return candidate.Apply(workspace, p)
}

// gitTimeout bounds one git invocation.
const gitTimeout = 30 * time.Second

func (h *Healer) git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = h.Workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
