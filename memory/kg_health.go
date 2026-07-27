package memory

// kg_health.go — structural analyses that fall out of the code graph once it
// holds defines/references/imports edges: what a change can break, what is
// unreachable, where the layering is circular, and what is both important and
// untested.
//
// These are questions no language server answers, because each needs a
// repository-wide view that persists between sessions. They are the practical
// payoff of maintaining an index rather than querying one.

import (
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/darkcode/core"
)

// Impact is the set of files a change can reach, and how far.
type Impact struct {
	Changed  []string `json:"changed"`  // the files asked about
	Symbols  []string `json:"symbols"`  // symbols they define
	Affected []string `json:"affected"` // files transitively depending on those symbols
	Depth    int      `json:"depth"`    // hops explored
	// Severity is Affected as a share of the indexed files, in [0,1]. It is
	// what a permission gate or plan preview can threshold on.
	Severity float64 `json:"severity"`
}

// BlastRadius reports which files depend on the symbols defined by the given
// files, following reference edges outward up to maxDepth hops.
//
// A one-hop radius answers "who calls what I am about to change". Deeper hops
// answer "how far does the shockwave travel" — useful before a refactor, and
// the number a plan preview should show the user before they approve it.
func (kg *KnowledgeGraph) BlastRadius(files []string, maxDepth int) Impact {
	if maxDepth < 1 {
		maxDepth = 1
	}
	imp := Impact{Changed: files, Depth: maxDepth}

	seenFiles := map[string]bool{}
	for _, f := range files {
		seenFiles[normalizeFileID(f)] = true
	}
	frontier := append([]string(nil), files...)
	symbolSeen := map[string]bool{}

	for hop := 0; hop < maxDepth && len(frontier) > 0; hop++ {
		var next []string
		for _, f := range frontier {
			fileID := normalizeFileID(f)
			for _, e := range kg.GetEdges(fileID) {
				// Symbols this file defines…
				if e.Relation != core.KGRelDefines || e.From != fileID {
					continue
				}
				if !symbolSeen[e.To] {
					symbolSeen[e.To] = true
					if n, ok := kg.GetNode(e.To); ok {
						imp.Symbols = append(imp.Symbols, n.Label)
					}
				}
				// …and every file that references them.
				for _, re := range kg.GetEdges(e.To) {
					if re.Relation != core.KGRelReferences || re.To != e.To {
						continue
					}
					if !seenFiles[re.From] {
						seenFiles[re.From] = true
						label := strings.TrimPrefix(re.From, "file:")
						imp.Affected = append(imp.Affected, label)
						next = append(next, label)
					}
				}
			}
		}
		frontier = next
	}

	sort.Strings(imp.Symbols)
	sort.Strings(imp.Affected)
	if total := len(kg.FindByType(core.KGNodeFile)); total > 0 {
		imp.Severity = float64(len(imp.Affected)) / float64(total)
	}
	return imp
}

// normalizeFileID accepts either a bare path or an already-prefixed node id.
func normalizeFileID(f string) string {
	if strings.HasPrefix(f, "file:") {
		return f
	}
	return "file:" + f
}

// Finding is one repository-health issue, ranked by severity.
type Finding struct {
	Kind    string  `json:"kind"`
	Subject string  `json:"subject"`
	Detail  string  `json:"detail"`
	Weight  float64 `json:"weight"` // 0..1, higher is worse
}

// entryPoints are never dead even with no in-repo references: the toolchain,
// not the code, calls them.
func isEntryPoint(label, kind string) bool {
	switch label {
	case "main", "init", "String", "Error":
		return true
	}
	return strings.HasPrefix(label, "Test") || strings.HasPrefix(label, "Benchmark") ||
		strings.HasPrefix(label, "Fuzz") || strings.HasPrefix(label, "Example") ||
		kind == "interface"
}

// DeadSymbols returns symbols no other indexed file mentions. Reachability
// here is "referenced by name anywhere else in the repository", so anything
// reached only through reflection, a plugin boundary, or an exported API used
// downstream will show up — the list is a starting point for review, not a
// delete list.
func (kg *KnowledgeGraph) DeadSymbols() []Finding {
	var out []Finding
	for _, n := range kg.FindByType(core.KGNodeSymbol) {
		if n.Properties["origin"] != "code_index" {
			continue
		}
		if isEntryPoint(n.Label, n.Properties["kind"]) {
			continue
		}
		if refs, _ := strconv.Atoi(n.Properties["references"]); refs > 0 {
			continue
		}
		out = append(out, Finding{
			Kind: "dead-code", Subject: n.Label,
			Detail: "no other indexed file references it (" + n.Provenance + ")",
			Weight: 0.3,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// Cycles returns import cycles between packages. A cycle is the clearest
// possible signal that a layering boundary has been crossed, and it is
// invisible from any single file.
func (kg *KnowledgeGraph) Cycles() []Finding {
	var out []Finding
	for _, cycle := range FindCycles(kg.packageGraph()) {
		out = append(out, Finding{
			Kind: "import-cycle", Subject: cycle[0],
			Detail: strings.Join(cycle, " → "),
			Weight: 0.9,
		})
	}
	return out
}

// FindCycles returns every cycle in a directed graph, each as the node path
// that closes it (first node repeated at the end). Output is deterministic so
// two runs — or two commits — can be compared.
func FindCycles(graph map[string][]string) [][]string {
	// Colour-marking DFS: grey means "on the current path", so meeting a grey
	// node closes a cycle.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	state := map[string]int{}
	var stack []string
	var cycles [][]string

	var visit func(string)
	visit = func(node string) {
		state[node] = grey
		stack = append(stack, node)
		for _, dep := range graph[node] {
			switch state[dep] {
			case white:
				visit(dep)
			case grey:
				// Report the cycle from where it was entered.
				for i, p := range stack {
					if p == dep {
						cycles = append(cycles, append(append([]string{}, stack[i:]...), dep))
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = black
	}

	nodes := make([]string, 0, len(graph))
	for n := range graph {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes) // deterministic output
	for _, n := range nodes {
		if state[n] == white {
			visit(n)
		}
	}
	return cycles
}

// packageGraph derives package → package edges from the file → package import
// edges. A file's package is its directory; an import resolves to a local
// package when its path ends with one of the directories in the repository,
// which is what makes this work without knowing the module path.
func (kg *KnowledgeGraph) packageGraph() map[string][]string {
	localDirs := map[string]bool{}
	for _, n := range kg.FindByType(core.KGNodeFile) {
		localDirs[path.Dir(n.Label)] = true
	}

	graph := map[string][]string{}
	seen := map[string]bool{}
	for _, n := range kg.FindByType(core.KGNodeFile) {
		from := path.Dir(n.Label)
		for _, e := range kg.GetEdges(n.ID) {
			if e.Relation != core.KGRelImports || e.From != n.ID {
				continue
			}
			to := resolveLocal(strings.TrimPrefix(e.To, "package:"), localDirs)
			if to == "" || to == from {
				continue
			}
			if key := from + "\x00" + to; !seen[key] {
				seen[key] = true
				graph[from] = append(graph[from], to)
			}
		}
	}
	return graph
}

// resolveLocal maps an import path to a repository directory when one matches
// its tail, else "" for an external dependency.
func resolveLocal(importPath string, localDirs map[string]bool) string {
	for dir := importPath; dir != "" && dir != "." && dir != "/"; dir = trimFirstSegment(dir) {
		if localDirs[dir] {
			return dir
		}
	}
	return ""
}

func trimFirstSegment(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return ""
}

// UntestedHotspots returns the most-referenced symbols that no test file
// mentions. Ranking by fan-in is what makes this actionable: it names the code
// most likely to break the most callers, rather than listing everything
// uncovered.
func (kg *KnowledgeGraph) UntestedHotspots(limit int) []Finding {
	var out []Finding
	for _, n := range kg.FindByType(core.KGNodeSymbol) {
		if n.Properties["origin"] != "code_index" || isEntryPoint(n.Label, n.Properties["kind"]) {
			continue
		}
		refs, _ := strconv.Atoi(n.Properties["references"])
		if refs < hotspotMinRefs {
			continue // barely used; not a hotspot
		}
		tested := false
		for _, e := range kg.GetEdges(n.ID) {
			if e.Relation == core.KGRelReferences && isTestFile(strings.TrimPrefix(e.From, "file:")) {
				tested = true
				break
			}
		}
		if tested {
			continue
		}
		out = append(out, Finding{
			Kind: "untested-hotspot", Subject: n.Label,
			Detail: strconv.Itoa(refs) + " referencing file(s), none of them tests (" + n.Provenance + ")",
			// Weight rises with fan-in but saturates, so one very popular
			// symbol cannot dominate the whole health score.
			Weight: min(1.0, float64(refs)/10.0),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func isTestFile(p string) bool {
	base := path.Base(p)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains(p, "/tests/") || strings.Contains(p, "/__tests__/")
}

// hotspotMinRefs is the fan-in at which an untested symbol is worth naming.
// Below it, "untested" describes most of a healthy codebase and the signal is
// buried in noise.
const hotspotMinRefs = 5

// findingsPerKind caps how many examples of each issue the report carries.
// The counts convey scale; a thousand individual entries convey nothing.
const findingsPerKind = 10

// HealthReport aggregates the structural findings into one ranked view.
type HealthReport struct {
	Score   float64 `json:"score"` // 0..100, higher is healthier
	Files   int     `json:"files"`
	Symbols int     `json:"symbols"`
	// Counts is the total per issue kind, independent of how many examples
	// Findings carries.
	Counts   map[string]int `json:"counts"`
	Findings []Finding      `json:"findings"`
}

// Health scores the repository's structure and returns the worst examples of
// each issue, ranked. The score is a blunt instrument by design — its job is
// to move when something structural degrades, so a trend is readable.
//
// Reference counting matches symbols by name, so two same-named methods in
// different packages share a count. That inflates fan-in for common names like
// Stop or Close; the ranking is a triage aid, not a measurement.
func (kg *KnowledgeGraph) Health() HealthReport {
	rep := HealthReport{
		Files:   len(kg.FindByType(core.KGNodeFile)),
		Symbols: len(kg.FindByType(core.KGNodeSymbol)),
		Counts:  map[string]int{},
	}

	var penalty float64
	for _, group := range [][]Finding{kg.Cycles(), kg.UntestedHotspots(0), kg.DeadSymbols()} {
		if len(group) == 0 {
			continue
		}
		rep.Counts[group[0].Kind] = len(group)
		for _, f := range group {
			penalty += f.Weight
		}
		if len(group) > findingsPerKind {
			group = group[:findingsPerKind]
		}
		rep.Findings = append(rep.Findings, group...)
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		return rep.Findings[i].Weight > rep.Findings[j].Weight
	})

	// Penalty is scaled by repository size so a large codebase is not punished
	// simply for having more of everything.
	rep.Score = 100 * (1 - min(1.0, penalty/float64(max(rep.Symbols, 1))*4))
	return rep
}
