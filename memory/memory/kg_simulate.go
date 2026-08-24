package memory

// kg_simulate.go — architecture simulation.
//
// Architectural decisions get made in documents and discovered to be wrong in
// production. With a package graph already in hand, a proposed change can be
// applied to a copy and measured before anyone writes code: does splitting
// this package remove the cycle, or just move it?
//
// The metrics are the standard structural ones — coupling, cycles, dependency
// depth — computed the same way before and after, so the delta is meaningful
// even where the absolute numbers are only indicative.

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Metrics describes the shape of a package graph.
type Metrics struct {
	Packages int `json:"packages"`
	Edges    int `json:"edges"`
	Cycles   int `json:"cycles"`
	// MaxDepth is the longest dependency chain. A deep graph is slow to build
	// and hard to reason about.
	MaxDepth int `json:"max_depth"`
	// MaxFanIn is the most depended-upon package — the one whose change costs
	// the most.
	MaxFanIn    int    `json:"max_fan_in"`
	MaxFanInPkg string `json:"max_fan_in_pkg,omitempty"`
	// AvgCoupling is edges per package: how entangled the design is on average.
	AvgCoupling float64 `json:"avg_coupling"`
}

// Change is one proposed architectural edit.
//
// The vocabulary is deliberately small. These three cover the decisions people
// actually argue about — splitting a package, removing a dependency, and
// inverting one — and each maps to an unambiguous graph transformation. A
// richer language would be harder to specify than the code it replaces.
type Change struct {
	// Kind is "split", "remove_dependency" or "invert_dependency".
	Kind string `json:"kind"`
	// Package is the subject; From/To name the edge for dependency changes.
	Package string `json:"package,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	// Into names the new packages for a split, and which existing dependencies
	// move to the first of them.
	Into  []string `json:"into,omitempty"`
	Moves []string `json:"moves,omitempty"`
}

// Simulation is the before/after comparison of a proposal.
type Simulation struct {
	Change  Change   `json:"change"`
	Before  Metrics  `json:"before"`
	After   Metrics  `json:"after"`
	Verdict string   `json:"verdict"`
	Notes   []string `json:"notes"`
}

// Simulate applies a proposed change to a copy of the package graph and
// reports what it would do to the repository's structure.
func (kg *KnowledgeGraph) Simulate(change Change) (*Simulation, error) {
	before := kg.packageGraph()
	after, notes, err := applyChange(before, change)
	if err != nil {
		return nil, err
	}

	sim := &Simulation{
		Change: change,
		Before: measure(before),
		After:  measure(after),
		Notes:  notes,
	}
	sim.Verdict = verdict(sim.Before, sim.After)
	return sim, nil
}

// applyChange returns a transformed copy of the graph. The original is never
// mutated — a simulation that changed the real graph would be a very bad
// surprise.
func applyChange(graph map[string][]string, c Change) (map[string][]string, []string, error) {
	out := make(map[string][]string, len(graph))
	for pkg, deps := range graph {
		out[pkg] = append([]string(nil), deps...)
	}
	var notes []string

	switch c.Kind {
	case "remove_dependency":
		if c.From == "" || c.To == "" {
			return nil, nil, fmt.Errorf("remove_dependency needs from and to")
		}
		if !hasEdge(out, c.From, c.To) {
			return nil, nil, fmt.Errorf("%s does not import %s", c.From, c.To)
		}
		out[c.From] = without(out[c.From], c.To)
		notes = append(notes, fmt.Sprintf("removed %s → %s", c.From, c.To))

	case "invert_dependency":
		if c.From == "" || c.To == "" {
			return nil, nil, fmt.Errorf("invert_dependency needs from and to")
		}
		if !hasEdge(out, c.From, c.To) {
			return nil, nil, fmt.Errorf("%s does not import %s", c.From, c.To)
		}
		out[c.From] = without(out[c.From], c.To)
		out[c.To] = appendUnique(out[c.To], c.From)
		notes = append(notes, fmt.Sprintf("inverted %s → %s into %s → %s", c.From, c.To, c.To, c.From))

	case "split":
		if c.Package == "" || len(c.Into) < 2 {
			return nil, nil, fmt.Errorf("split needs package and at least two names in into")
		}
		deps, ok := out[c.Package]
		if !ok {
			return nil, nil, fmt.Errorf("unknown package %s", c.Package)
		}
		// Dependencies named in Moves go to the first new package; the rest
		// stay with the second. Anything that imported the original now
		// imports both, which is the honest pessimistic assumption: without
		// knowing which symbols a caller used, a split cannot reduce its
		// dependencies.
		first, second := c.Into[0], c.Into[1]
		moved := map[string]bool{}
		for _, m := range c.Moves {
			moved[m] = true
		}
		var firstDeps, secondDeps []string
		for _, d := range deps {
			if moved[d] {
				firstDeps = append(firstDeps, d)
			} else {
				secondDeps = append(secondDeps, d)
			}
		}
		delete(out, c.Package)
		out[first], out[second] = firstDeps, secondDeps
		for pkg, ds := range out {
			if !contains(ds, c.Package) {
				continue
			}
			ds = without(ds, c.Package)
			ds = appendUnique(ds, first)
			ds = appendUnique(ds, second)
			out[pkg] = ds
		}
		notes = append(notes, fmt.Sprintf("split %s into %s and %s", c.Package, first, second),
			"callers are assumed to depend on both halves — a real split usually does better")

	default:
		return nil, nil, fmt.Errorf("unknown change %q (want split, remove_dependency, invert_dependency)", c.Kind)
	}
	return out, notes, nil
}

// measure computes the structural metrics of a package graph.
func measure(graph map[string][]string) Metrics {
	m := Metrics{Packages: len(graph)}

	fanIn := map[string]int{}
	for _, deps := range graph {
		m.Edges += len(deps)
		for _, d := range deps {
			fanIn[d]++
		}
	}
	// Deterministic tie-breaking so the same graph always names the same
	// package as most-depended-upon.
	pkgs := make([]string, 0, len(fanIn))
	for p := range fanIn {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		if fanIn[p] > m.MaxFanIn {
			m.MaxFanIn, m.MaxFanInPkg = fanIn[p], p
		}
	}
	if m.Packages > 0 {
		m.AvgCoupling = float64(m.Edges) / float64(m.Packages)
	}
	m.Cycles = len(FindCycles(graph))
	m.MaxDepth = maxDepth(graph)
	return m
}

// maxDepth is the longest dependency chain, measured with memoised DFS. Nodes
// on a cycle are bounded by the visit set rather than recursing forever.
func maxDepth(graph map[string][]string) int {
	depth := map[string]int{}
	onPath := map[string]bool{}

	var walk func(string) int
	walk = func(pkg string) int {
		if d, done := depth[pkg]; done {
			return d
		}
		if onPath[pkg] {
			return 0 // a cycle contributes no further depth
		}
		onPath[pkg] = true
		best := 0
		for _, dep := range graph[pkg] {
			if d := walk(dep) + 1; d > best {
				best = d
			}
		}
		onPath[pkg] = false
		depth[pkg] = best
		return best
	}

	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	best := 0
	for _, p := range pkgs {
		if d := walk(p); d > best {
			best = d
		}
	}
	return best
}

// verdict summarises the deltas in the order that matters: cycles first,
// because a cycle is a hard structural fault, then coupling and depth.
func verdict(before, after Metrics) string {
	switch {
	case after.Cycles < before.Cycles:
		return fmt.Sprintf("improves the design — removes %d cycle(s)", before.Cycles-after.Cycles)
	case after.Cycles > before.Cycles:
		return fmt.Sprintf("makes it worse — introduces %d cycle(s)", after.Cycles-before.Cycles)
	}
	switch {
	case after.AvgCoupling < before.AvgCoupling-0.01:
		return fmt.Sprintf("modest improvement — coupling %.2f → %.2f", before.AvgCoupling, after.AvgCoupling)
	case after.AvgCoupling > before.AvgCoupling+0.01:
		return fmt.Sprintf("adds coupling — %.2f → %.2f", before.AvgCoupling, after.AvgCoupling)
	case after.MaxDepth < before.MaxDepth:
		return fmt.Sprintf("shortens the longest dependency chain %d → %d", before.MaxDepth, after.MaxDepth)
	case after.MaxDepth > before.MaxDepth:
		return fmt.Sprintf("lengthens the longest dependency chain %d → %d", before.MaxDepth, after.MaxDepth)
	}
	return "structurally neutral — no measurable change"
}

// Format renders a simulation for a human or a model.
func (s *Simulation) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", s.Verdict)
	fmt.Fprintf(&b, "%-22s %8s %8s\n", "metric", "before", "after")
	row := func(name string, before, after interface{}) {
		fmt.Fprintf(&b, "%-22s %8v %8v\n", name, before, after)
	}
	row("packages", s.Before.Packages, s.After.Packages)
	row("dependencies", s.Before.Edges, s.After.Edges)
	row("cycles", s.Before.Cycles, s.After.Cycles)
	row("max chain depth", s.Before.MaxDepth, s.After.MaxDepth)
	row("avg coupling", fmt.Sprintf("%.2f", s.Before.AvgCoupling), fmt.Sprintf("%.2f", s.After.AvgCoupling))
	row("most depended on", s.Before.MaxFanIn, s.After.MaxFanIn)
	for _, n := range s.Notes {
		fmt.Fprintf(&b, "\n· %s", n)
	}
	return b.String()
}

// --- small graph helpers ---

func hasEdge(graph map[string][]string, from, to string) bool { return contains(graph[from], to) }

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func without(list []string, drop string) []string {
	out := list[:0:0]
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func appendUnique(list []string, add string) []string {
	if contains(list, add) {
		return list
	}
	return append(list, add)
}

// PackageOf reports which package a file belongs to, so callers can name a
// package without knowing the graph's internal keying.
func PackageOf(file string) string { return path.Dir(file) }
