package memory

// kg_patterns.go — learning a repository's conventions, and carrying them
// between repositories (report #82, #83).
//
// Every codebase has rules nobody wrote down: handlers come with tests, the
// storage layer never imports the HTTP layer, anything named *Store lives
// under store/. They are enforced by review and forgotten at the boundary of
// whoever remembers them.
//
// The graph already holds the evidence. A convention is a statement that holds
// across most of the repository and fails in a few places, which is exactly
// the shape of an association rule: how often the premise occurs (support) and
// how often the conclusion follows (confidence). Mining them turns "the reviewer
// happened to notice" into "these eleven files break a rule the other ninety
// keep".
//
// Two guards keep this from generating noise. A rule needs real support before
// it is called a pattern — three coincidences are not a convention. And a rule
// with no exceptions is not reported as a finding, because a rule nothing
// violates tells you nothing you need to act on.
//
// The library persists outside any one repository, which is what makes the
// cross-repository case work: conventions mined where they are followed can be
// checked where they are not yet.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkcode/core"
)

// Pattern is one mined convention.
type Pattern struct {
	// Kind is "test-companion" or "layering".
	Kind string `json:"kind"`
	// Subject is the premise: the package or file class the rule applies to.
	Subject string `json:"subject"`
	// Expect is the conclusion that usually follows.
	Expect string `json:"expect"`
	// Support is how many cases match the premise. A rule needs enough of
	// these to be a convention rather than a coincidence.
	Support int `json:"support"`
	// Holds is how many of those also match the conclusion.
	Holds int `json:"holds"`
	// Origin names the repository this was mined from, so a violation found
	// elsewhere can say where the rule came from.
	Origin string `json:"origin,omitempty"`
}

// Confidence is the share of matching cases where the rule holds, in [0,1].
func (p Pattern) Confidence() float64 {
	if p.Support == 0 {
		return 0
	}
	return float64(p.Holds) / float64(p.Support)
}

// Describe renders the rule as a sentence.
func (p Pattern) Describe() string {
	switch p.Kind {
	case "test-companion":
		return fmt.Sprintf("files in %s have tests (%d of %d)", p.Subject, p.Holds, p.Support)
	case "layering":
		return fmt.Sprintf("%s does not import %s (%d of %d files)", p.Subject, p.Expect, p.Holds, p.Support)
	}
	return p.Subject + " → " + p.Expect
}

// Violation is a case that breaks a pattern.
type Violation struct {
	Pattern Pattern `json:"pattern"`
	File    string  `json:"file"`
	Detail  string  `json:"detail"`
}

// minSupport is how many cases a rule needs before it counts as a convention.
// Below this, "every one of the two files in this package has a test" is
// arithmetic, not a rule.
const minSupport = 5

// minPatternConfidence is how consistently a rule must hold to be worth mining. Set
// high because the value is in the exceptions: a rule kept 70% of the time
// describes a codebase with two conventions, not one with violations.
const minPatternConfidence = 0.8

// MinePatterns extracts the conventions a repository actually follows.
//
// origin labels where they came from, so the library can attribute a rule when
// it is applied to a different repository.
func (kg *KnowledgeGraph) MinePatterns(origin string) []Pattern {
	var out []Pattern
	out = append(out, kg.mineTestCompanions(origin)...)
	out = append(out, kg.mineLayering(origin)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// mineTestCompanions finds packages where source files usually have tests.
func (kg *KnowledgeGraph) mineTestCompanions(origin string) []Pattern {
	type counts struct{ total, tested int }
	byPkg := map[string]*counts{}

	files := kg.FindByType(core.KGNodeFile)
	tested := map[string]bool{}
	for _, n := range files {
		if isTestFile(n.Label) {
			// foo_test.go covers foo.go; the base name is the link.
			tested[testSubject(n.Label)] = true
		}
	}
	for _, n := range files {
		if isTestFile(n.Label) {
			continue
		}
		pkg := path.Dir(n.Label)
		c := byPkg[pkg]
		if c == nil {
			c = &counts{}
			byPkg[pkg] = c
		}
		c.total++
		if tested[n.Label] {
			c.tested++
		}
	}

	var out []Pattern
	for pkg, c := range byPkg {
		p := Pattern{Kind: "test-companion", Subject: pkg, Expect: "a test file",
			Support: c.total, Holds: c.tested, Origin: origin}
		if p.Support >= minSupport && p.Confidence() >= minPatternConfidence {
			out = append(out, p)
		}
	}
	return out
}

// testSubject maps a test file back to the file it covers.
func testSubject(p string) string {
	dir, base := path.Dir(p), path.Base(p)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		base = strings.TrimSuffix(base, "_test.go") + ".go"
	case strings.HasPrefix(base, "test_"):
		base = strings.TrimPrefix(base, "test_")
	case strings.Contains(base, ".test."):
		base = strings.Replace(base, ".test.", ".", 1)
	case strings.Contains(base, ".spec."):
		base = strings.Replace(base, ".spec.", ".", 1)
	default:
		return p
	}
	return path.Join(dir, base)
}

// mineLayering finds the direction boundaries are kept in.
//
// The rule is derived from the edges that exist rather than from every pair
// that does not: if http imports store, then the kept boundary is that store
// does not import http. That is what layering means — the dependency runs one
// way — and it is the shape that matters, because the valuable rule is usually
// "the lower layer must not reach back up to the top one".
//
// Deriving it from existing edges also keeps the output linear in the number
// of dependencies. Enumerating absent pairs instead would be quadratic in
// package count, and nearly all of those pairs are absent because the two
// packages have nothing to do with each other, not because anyone drew a line.
func (kg *KnowledgeGraph) mineLayering(origin string) []Pattern {
	graph := kg.packageGraph()

	filesPerPkg := map[string]int{}
	for _, n := range kg.FindByType(core.KGNodeFile) {
		if !isTestFile(n.Label) {
			filesPerPkg[path.Dir(n.Label)]++
		}
	}

	var out []Pattern
	for pkg, deps := range graph {
		for _, target := range deps {
			// pkg imports target, so the boundary is the reverse direction.
			if contains(graph[target], pkg) {
				continue // imports run both ways already: no boundary is being kept
			}
			total := filesPerPkg[target]
			if total < minSupport {
				continue // too small for "none of its files do this" to mean much
			}
			out = append(out, Pattern{
				Kind: "layering", Subject: target, Expect: pkg,
				Support: total, Holds: total, Origin: origin,
			})
		}
	}
	// A repository with many packages produces a lot of these; keep the ones
	// about the most-depended-upon targets, which are the real boundaries.
	// Ties break on name because the candidates came out of map iteration, and
	// a library that stored a different fifty each run would be useless for
	// comparing one commit to the next.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Support != out[j].Support {
			return out[i].Support > out[j].Support
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Expect < out[j].Expect
	})
	// Cap per subject before capping overall. A single widely-used package
	// otherwise fills the entire library with its own boundaries, and fifty
	// rules about one package is worth less than a few rules about many.
	perSubject := map[string]int{}
	kept := out[:0]
	for _, p := range out {
		if perSubject[p.Subject] >= maxPerSubject {
			continue
		}
		perSubject[p.Subject]++
		kept = append(kept, p)
	}
	out = kept
	if len(out) > maxLayeringPatterns {
		out = out[:maxLayeringPatterns]
	}
	return out
}

// maxLayeringPatterns bounds the whole set of boundary rules, and
// maxPerSubject bounds how many any one package may contribute.
const (
	maxLayeringPatterns = 50
	maxPerSubject       = 3
)

// CheckPatterns reports where this repository breaks the given patterns.
//
// This is the cross-repository case: patterns mined in one repository are
// applied to another, and the exceptions are what the caller wants.
func (kg *KnowledgeGraph) CheckPatterns(patterns []Pattern) []Violation {
	var out []Violation

	files := kg.FindByType(core.KGNodeFile)
	tested := map[string]bool{}
	byPkg := map[string][]string{}
	for _, n := range files {
		if isTestFile(n.Label) {
			tested[testSubject(n.Label)] = true
			continue
		}
		byPkg[path.Dir(n.Label)] = append(byPkg[path.Dir(n.Label)], n.Label)
	}
	graph := kg.packageGraph()

	for _, p := range patterns {
		switch p.Kind {
		case "test-companion":
			for _, f := range byPkg[p.Subject] {
				if !tested[f] {
					out = append(out, Violation{Pattern: p, File: f,
						Detail: "no test file, though " + p.Describe()})
				}
			}
		case "layering":
			if contains(graph[p.Subject], p.Expect) {
				out = append(out, Violation{Pattern: p, File: p.Subject,
					Detail: fmt.Sprintf("%s imports %s, which %s does not",
						p.Subject, p.Expect, originLabel(p))})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern.Kind != out[j].Pattern.Kind {
			return out[i].Pattern.Kind < out[j].Pattern.Kind
		}
		return out[i].File < out[j].File
	})
	return out
}

func originLabel(p Pattern) string {
	if p.Origin == "" {
		return "this convention"
	}
	return p.Origin
}

// PatternLibrary persists mined conventions across repositories.
//
// This is the whole point of #82: a rule learned where it is followed is only
// useful if it outlives the repository it came from.
type PatternLibrary struct {
	path string
	// byRepo keeps each repository's contribution separate, so re-mining one
	// replaces its rules rather than duplicating them.
	byRepo map[string][]Pattern
}

// NewPatternLibrary opens (or creates) a library under dir.
func NewPatternLibrary(dir string) *PatternLibrary {
	l := &PatternLibrary{byRepo: map[string][]Pattern{}}
	if dir != "" {
		l.path = filepath.Join(dir, "patterns.json")
		l.load()
	}
	return l
}

// Learn records the patterns mined from one repository, replacing whatever was
// previously known about it.
func (l *PatternLibrary) Learn(repo string, patterns []Pattern) {
	l.byRepo[repo] = patterns
	l.save()
}

// All returns every pattern the library holds, ordered for stable output.
func (l *PatternLibrary) All() []Pattern {
	repos := make([]string, 0, len(l.byRepo))
	for r := range l.byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	var out []Pattern
	for _, r := range repos {
		out = append(out, l.byRepo[r]...)
	}
	return out
}

// Elsewhere returns the patterns learned from every repository except this
// one — the set worth checking a new repository against.
func (l *PatternLibrary) Elsewhere(repo string) []Pattern {
	var out []Pattern
	repos := make([]string, 0, len(l.byRepo))
	for r := range l.byRepo {
		if r != repo {
			repos = append(repos, r)
		}
	}
	sort.Strings(repos)
	for _, r := range repos {
		out = append(out, l.byRepo[r]...)
	}
	return out
}

// Repos lists the repositories the library has learned from.
func (l *PatternLibrary) Repos() []string {
	out := make([]string, 0, len(l.byRepo))
	for r := range l.byRepo {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (l *PatternLibrary) save() {
	if l.path == "" {
		return
	}
	blob, err := json.MarshalIndent(l.byRepo, "", "  ")
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, blob, 0o600) == nil {
		_ = os.Rename(tmp, l.path)
	}
}

func (l *PatternLibrary) load() {
	blob, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	stored := map[string][]Pattern{}
	if json.Unmarshal(blob, &stored) == nil {
		l.byRepo = stored
	}
}
