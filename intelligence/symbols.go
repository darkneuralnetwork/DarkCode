package intelligence

// symbols.go — the symbol graph populated by AST scanning.
//
// SymbolGraph holds every top-level declaration (function / struct /
// interface / type / const / var) discovered across the workspace. It is the
// backbone of the deterministic toolchain + project intelligence (spec §7):
// find-references, go-to-definition, call-graph, and inheritance queries all
// resolve against it without an LLM.

import "strings"

// Symbol is one discovered declaration.
type Symbol struct {
	Name     string
	Kind     string // function | struct | interface | type | const | var
	FilePath string
	Line     int
	Receiver string // for methods: the receiver type name (e.g. "*Kernel")
}

// SymbolQuery is a deterministic lookup against the graph.
type SymbolQuery struct {
	Name string
	Kind string // optional kind filter
}

// SymbolGraph stores symbols and indexes them by name for O(1) lookup.
type SymbolGraph struct {
	byName map[string][]Symbol
	all    []Symbol
}

func NewSymbolGraph() *SymbolGraph {
	return &SymbolGraph{byName: map[string][]Symbol{}}
}

// Add inserts a symbol (idempotent on exact duplicates).
func (s *SymbolGraph) Add(sym Symbol) {
	for _, ex := range s.byName[sym.Name] {
		if ex.FilePath == sym.FilePath && ex.Line == sym.Line && ex.Kind == sym.Kind {
			return
		}
	}
	s.byName[sym.Name] = append(s.byName[sym.Name], sym)
	s.all = append(s.all, sym)
}

// Find returns symbols matching the query (name required; kind optional).
func (s *SymbolGraph) Find(q SymbolQuery) []Symbol {
	if q.Name == "" {
		return nil
	}
	matches := s.byName[q.Name]
	if q.Kind == "" {
		return matches
	}
	var out []Symbol
	for _, m := range matches {
		if m.Kind == q.Kind {
			out = append(out, m)
		}
	}
	return out
}

// All returns every symbol in the graph.
func (s *SymbolGraph) All() []Symbol { return s.all }

// ByFile returns symbols grouped by file path.
func (s *SymbolGraph) ByFile() map[string][]Symbol {
	out := map[string][]Symbol{}
	for _, sym := range s.all {
		out[sym.FilePath] = append(out[sym.FilePath], sym)
	}
	return out
}

// Names returns the distinct symbol names (sorted).
func (s *SymbolGraph) Names() []string {
	names := make([]string, 0, len(s.byName))
	for n := range s.byName {
		names = append(names, n)
	}
	// simple sort (avoid sort import churn here)
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && strings.Compare(names[j], names[j-1]) < 0; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// Clear empties the graph (used before a re-scan).
func (s *SymbolGraph) Clear() {
	s.byName = map[string][]Symbol{}
	s.all = nil
}

// Count returns the total symbol count.
func (s *SymbolGraph) Count() int { return len(s.all) }
