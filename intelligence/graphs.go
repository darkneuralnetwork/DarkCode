package intelligence

// graphs.go — real dependency / call / import / class graphs.
//
// Previously these were empty structs (spec §7 audit: "empty structs with no
// fields and no population logic"). They now hold real edges extracted from
// Go AST scans and support the deterministic queries the spec requires:
//   - DependencyGraph: package → packages it imports
//   - ImportGraph:      file → imported packages
//   - CallGraph:        caller function → callee names
//   - ClassGraph:       type → embedded types (Go "inheritance")

import "sort"

// DependencyGraph tracks package-level dependencies.
type DependencyGraph struct {
	// edges: importing package (dir or module path) -> set of imported packages
	edges map[string]map[string]bool
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{edges: map[string]map[string]bool{}}
}

// AddEdge records that `from` package imports `to`.
func (g *DependencyGraph) AddEdge(from, to string) {
	if g.edges[from] == nil {
		g.edges[from] = map[string]bool{}
	}
	g.edges[from][to] = true
}

// Imports returns the packages imported by `from` (sorted).
func (g *DependencyGraph) Imports(from string) []string {
	set := g.edges[from]
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ReverseDeps returns the packages that import `to` (sorted).
func (g *DependencyGraph) ReverseDeps(to string) []string {
	var out []string
	for from, set := range g.edges {
		if set[to] {
			out = append(out, from)
		}
	}
	sort.Strings(out)
	return out
}

// AllEdges returns every (from, to) edge — for serialization / visualization.
func (g *DependencyGraph) AllEdges() [][2]string {
	var out [][2]string
	for from, set := range g.edges {
		for to := range set {
			out = append(out, [2]string{from, to})
		}
	}
	return out
}

// PackageCount returns the number of distinct importing packages.
func (g *DependencyGraph) PackageCount() int { return len(g.edges) }

// ImportGraph tracks file-level imports.
type ImportGraph struct {
	// file -> imported packages
	edges map[string][]string
}

func NewImportGraph() *ImportGraph {
	return &ImportGraph{edges: map[string][]string{}}
}

// Add records that `file` imports `pkg`.
func (g *ImportGraph) Add(file, pkg string) {
	for _, ex := range g.edges[file] {
		if ex == pkg {
			return
		}
	}
	g.edges[file] = append(g.edges[file], pkg)
}

// Imports returns the packages imported by `file`.
func (g *ImportGraph) Imports(file string) []string { return g.edges[file] }

// Files returns all indexed files.
func (g *ImportGraph) Files() []string {
	out := make([]string, 0, len(g.edges))
	for f := range g.edges {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// FileCount returns the number of indexed files.
func (g *ImportGraph) FileCount() int { return len(g.edges) }

// CallEdge is one caller→callee relationship.
type CallEdge struct {
	Caller     string // "TypeName.Method" or "func"
	Callee     string // called identifier
	File       string
	Line       int
}

// CallGraph tracks function-call relationships.
type CallGraph struct {
	edges []CallEdge
	// index: caller -> []edge for fast lookup
	byCaller map[string][]CallEdge
}

func NewCallGraph() *CallGraph {
	return &CallGraph{byCaller: map[string][]CallEdge{}}
}

// Add records a call edge.
func (g *CallGraph) Add(e CallEdge) {
	g.edges = append(g.edges, e)
	g.byCaller[e.Caller] = append(g.byCaller[e.Caller], e)
}

// CallsBy returns the callees invoked from `caller`.
func (g *CallGraph) CallsBy(caller string) []CallEdge { return g.byCaller[caller] }

// AllEdges returns every call edge.
func (g *CallGraph) AllEdges() []CallEdge { return g.edges }

// EdgeCount returns the total call-edge count.
func (g *CallGraph) EdgeCount() int { return len(g.edges) }

// ClassGraph tracks Go type composition (embedding = "inheritance").
type ClassGraph struct {
	// type -> embedded types
	embeds map[string][]string
	// type -> kind (struct|interface)
	kinds map[string]string
}

func NewClassGraph() *ClassGraph {
	return &ClassGraph{embeds: map[string][]string{}, kinds: map[string]string{}}
}

// AddType records a type declaration.
func (g *ClassGraph) AddType(name, kind string) { g.kinds[name] = kind }

// AddEmbed records that `typ` embeds `embedded`.
func (g *ClassGraph) AddEmbed(typ, embedded string) {
	for _, ex := range g.embeds[typ] {
		if ex == embedded {
			return
		}
	}
	g.embeds[typ] = append(g.embeds[typ], embedded)
}

// Embeds returns the types embedded by `typ`.
func (g *ClassGraph) Embeds(typ string) []string { return g.embeds[typ] }

// Ancestors returns the full transitive set of embedded types for `typ`.
func (g *ClassGraph) Ancestors(typ string) []string {
	seen := map[string]bool{}
	var stack []string
	var walk func(t string)
	walk = func(t string) {
		for _, e := range g.embeds[t] {
			if seen[e] {
				continue
			}
			seen[e] = true
			stack = append(stack, e)
			walk(e)
		}
	}
	walk(typ)
	sort.Strings(stack)
	return stack
}

// Types returns all declared types.
func (g *ClassGraph) Types() []string {
	out := make([]string, 0, len(g.kinds))
	for t := range g.kinds {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Kind returns the kind (struct|interface) of a type, or "".
func (g *ClassGraph) Kind(typ string) string { return g.kinds[typ] }

// TypeCount returns the number of declared types.
func (g *ClassGraph) TypeCount() int { return len(g.kinds) }
