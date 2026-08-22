package memory

// kg_adjudicate.go — settling disagreements with evidence instead of opinion.
//
// A consensus round today asks several models and has one of them synthesise
// the result. That aggregates opinions: if the loudest model is confidently
// wrong, the synthesiser has no way to tell.
//
// The graph does. A coding answer makes checkable claims — this symbol exists,
// it lives in that file, this package imports that one — and each can be
// checked mechanically. Weighting a candidate by how many of its claims
// survive replaces "which answer sounds best" with "which answer is true about
// this repository".
//
// The bar is deliberately conservative. Only claims the graph can actually
// settle are checked, an unrecognised claim counts neither for nor against,
// and a repository the graph has not indexed produces no verdict at all —
// a confident score computed from nothing would be worse than no score.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/darkcode/infra/core"
)

// ClaimCheck is one verifiable assertion and what the graph said about it.
type ClaimCheck struct {
	Claim    string `json:"claim"`
	Kind     string `json:"kind"` // symbol-exists | symbol-location | dependency
	Verified bool   `json:"verified"`
	Detail   string `json:"detail"`
}

// Support is how well the graph backs a candidate answer.
type Support struct {
	Checked  int          `json:"checked"`
	Verified int          `json:"verified"`
	Checks   []ClaimCheck `json:"checks,omitempty"`
}

// Score is the share of checkable claims that survived, in [0,1]. An answer
// with nothing checkable scores 0.5: neither endorsed nor penalised, because
// making no verifiable claim is not the same as making a false one.
func (s Support) Score() float64 {
	if s.Checked == 0 {
		return 0.5
	}
	return float64(s.Verified) / float64(s.Checked)
}

// Contradicted lists the claims the graph refuted, which is the part worth
// showing a user.
func (s Support) Contradicted() []ClaimCheck {
	var out []ClaimCheck
	for _, c := range s.Checks {
		if !c.Verified {
			out = append(out, c)
		}
	}
	return out
}

// Claim patterns. Each captures a subject and, where relevant, an object.
// They are narrow on purpose: a loose pattern turns ordinary prose into
// "claims" and the score stops meaning anything.
var (
	// "Foo is defined in bar.go", "Foo lives in bar/baz.go"
	locationClaim = regexp.MustCompile(`\b([A-Za-z_][\w.]*)\s+is\s+(?:defined|declared|implemented)\s+in\s+` + "`?" + `([\w./-]+\.\w+)` + "`?")
	// "package a imports b", "a depends on b"
	dependencyClaim = regexp.MustCompile(`(?i)\b(?:package\s+)?` + "`?" + `([\w./-]+)` + "`?" + `\s+(?:imports|depends\s+on)\s+` + "`?" + `([\w./-]+)` + "`?")
	// "the Foo function", "the Bar struct" — an existence claim about a symbol.
	existenceClaim = regexp.MustCompile(`\bthe\s+` + "`?" + `([A-Z][\w]*)` + "`?" + `\s+(?:function|method|struct|type|interface)\b`)
)

// Adjudicate checks an answer's factual claims against the graph.
//
// It returns a zero Support when the graph holds no code index: an answer
// cannot be scored against knowledge that does not exist, and pretending
// otherwise would let an unindexed repository silently rank candidates at
// random.
func (kg *KnowledgeGraph) Adjudicate(answer string) Support {
	symbols := kg.symbolIndex()
	if len(symbols) == 0 {
		return Support{}
	}

	var s Support
	seen := map[string]bool{}
	add := func(c ClaimCheck) {
		if seen[c.Claim] {
			return // one verdict per distinct claim
		}
		seen[c.Claim] = true
		s.Checked++
		if c.Verified {
			s.Verified++
		}
		s.Checks = append(s.Checks, c)
	}

	for _, m := range locationClaim.FindAllStringSubmatch(answer, -1) {
		name, file := m[1], m[2]
		files, known := symbols[bareName(name)]
		switch {
		case !known:
			add(ClaimCheck{m[0], "symbol-location", false,
				fmt.Sprintf("no symbol named %s is indexed", name)})
		case matchesAnyFile(files, file):
			add(ClaimCheck{m[0], "symbol-location", true, name + " is indexed in " + file})
		default:
			add(ClaimCheck{m[0], "symbol-location", false,
				fmt.Sprintf("%s is indexed in %s, not %s", name, strings.Join(files, ", "), file)})
		}
	}

	for _, m := range existenceClaim.FindAllStringSubmatch(answer, -1) {
		name := m[1]
		if _, known := symbols[name]; known {
			add(ClaimCheck{m[0], "symbol-exists", true, name + " is indexed"})
		} else {
			add(ClaimCheck{m[0], "symbol-exists", false, "no symbol named " + name + " is indexed"})
		}
	}

	graph := kg.packageGraph()
	for _, m := range dependencyClaim.FindAllStringSubmatch(answer, -1) {
		from, to := m[1], m[2]
		fromPkg, toPkg := resolvePkg(graph, from), resolvePkg(graph, to)
		if fromPkg == "" || toPkg == "" {
			continue // not packages this repository has; not our claim to judge
		}
		if contains(graph[fromPkg], toPkg) {
			add(ClaimCheck{m[0], "dependency", true, fromPkg + " does import " + toPkg})
		} else {
			add(ClaimCheck{m[0], "dependency", false, fromPkg + " does not import " + toPkg})
		}
	}
	return s
}

// symbolIndex maps each indexed symbol name to the files defining it.
func (kg *KnowledgeGraph) symbolIndex() map[string][]string {
	out := map[string][]string{}
	for _, n := range kg.FindByType(core.KGNodeSymbol) {
		if n.Properties["origin"] != "code_index" {
			continue
		}
		// Provenance is "file:line"; the file is what a location claim names.
		file, _, _ := strings.Cut(n.Provenance, ":")
		if file != "" {
			out[n.Label] = appendUnique(out[n.Label], file)
		}
	}
	for name := range out {
		sort.Strings(out[name])
	}
	return out
}

// bareName strips a qualifier so "pkg.Foo" is checked as "Foo" — the graph
// indexes symbols by their own name, not by call-site spelling.
func bareName(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	return s
}

// matchesAnyFile reports whether a claimed path names one of the files. A
// claim may give a bare name or a partial path, so suffix matching is the
// honest comparison.
func matchesAnyFile(files []string, claimed string) bool {
	for _, f := range files {
		if f == claimed || strings.HasSuffix(f, "/"+claimed) || strings.HasSuffix(claimed, "/"+f) {
			return true
		}
	}
	return false
}

// resolvePkg maps a name in prose to a package in the graph, allowing the
// short form ("memory") for a nested path ("internal/memory").
func resolvePkg(graph map[string][]string, name string) string {
	if _, ok := graph[name]; ok {
		return name
	}
	for pkg := range graph {
		if pkg == name || strings.HasSuffix(pkg, "/"+name) {
			return pkg
		}
	}
	return ""
}

// AdjudicateCandidates scores several candidate answers and returns the index
// of the best-supported one, plus every score.
//
// Ties keep the first candidate, which is the primary model's answer — when
// the evidence does not distinguish them, the existing ordering should stand
// rather than being churned by noise.
func (kg *KnowledgeGraph) AdjudicateCandidates(candidates []string) (best int, supports []Support) {
	if len(candidates) == 0 {
		return -1, nil
	}
	supports = make([]Support, len(candidates))
	bestScore := -1.0
	for i, c := range candidates {
		supports[i] = kg.Adjudicate(c)
		if score := supports[i].Score(); score > bestScore {
			best, bestScore = i, score
		}
	}
	return best, supports
}
