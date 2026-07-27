package deterministic

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/intelligence"
	"github.com/darkcode/memory"
	"github.com/darkcode/tools"
)

// SyncStats summarizes one workspace→KG sync.
type SyncStats struct {
	Files    int `json:"files"`
	Symbols  int `json:"symbols"`
	Packages int `json:"packages"`
	Edges    int `json:"edges"`
}

// SyncWorkspaceKG scans the source under root and records symbols, imports,
// and reference counts in the knowledge graph as typed facts with provenance.
// Go is parsed exactly by go/ast; TypeScript/JavaScript, Python, Rust and Java
// are scanned by the pattern parser and produce the same node and edge shapes,
// so callers never branch on language. It is idempotent: nodes are upserted by
// ID and the KG's AddEdge reinforces rather than duplicates, so periodic
// re-syncs keep the graph fresh without growing it.
func SyncWorkspaceKG(ctx context.Context, root string, kg core.KnowledgeGraphStore) (SyncStats, error) {
	var stats SyncStats
	if kg == nil {
		return stats, fmt.Errorf("nil knowledge graph")
	}
	// A workspace with no Go files still gets indexed by the second pass, so
	// this must not return early.
	files := collectGoFiles(root)
	now := time.Now()
	fset := token.NewFileSet()

	// relPath keeps node IDs stable regardless of the cwd the scan ran from.
	relPath := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil {
			return filepath.ToSlash(r)
		}
		return filepath.ToSlash(p)
	}

	// Pass 1: per-file identifier sets (for reference fan-in counting) —
	// collected in the same parse pass used for definitions/imports below
	// would need AST retention; a second cheap parse keeps memory flat.
	identsByFile := make(map[string]map[string]bool, len(files))

	type symbolFact struct {
		def      definition
		rel      string   // defining file, relative
		refs     int      // number of OTHER files mentioning the identifier
		refFiles []string // which files those are, for reverse lookup
	}
	var symbols []symbolFact
	importsByFile := make(map[string][]importEntry)

	for _, f := range files {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		rel := relPath(f.path)

		parsed, err := parser.ParseFile(fset, f.path, f.src, 0)
		if err != nil {
			continue
		}
		idents := make(map[string]bool)
		ast.Inspect(parsed, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				idents[id.Name] = true
			}
			return true
		})
		identsByFile[rel] = idents

		for _, d := range parseDefinitions(fset, f) {
			symbols = append(symbols, symbolFact{def: d, rel: rel})
		}
		if imps := parseImports(fset, f); len(imps) > 0 {
			importsByFile[rel] = imps
		}
	}

	// Reference fan-in: count files (other than the defining one) that
	// mention each defined symbol name.
	for i := range symbols {
		name := symbols[i].def.Name
		for rel, idents := range identsByFile {
			if rel == symbols[i].rel {
				continue
			}
			if idents[name] {
				symbols[i].refs++
				symbols[i].refFiles = append(symbols[i].refFiles, rel)
			}
		}
	}

	// Version the index against the revision it was read at, so the graph can
	// later report which of its beliefs predate the current HEAD. Empty
	// outside a git repository — an unversioned fact is better than a fake one.
	head := memory.GitHead(root)

	// Write facts. File nodes first so edges always resolve.
	seenFiles := make(map[string]bool)
	addFileNode := func(rel string) {
		if seenFiles[rel] {
			return
		}
		seenFiles[rel] = true
		props := map[string]string{"origin": "code_index"}
		if head != "" {
			props["commit"] = head
		}
		_ = kg.AddNode(&core.KGNode{
			ID:         "file:" + rel,
			Label:      rel,
			Type:       core.KGNodeFile,
			Properties: props,
			Provenance: rel,
			Confidence: 1.0,
			LastSeen:   now,
		})
		stats.Files++
	}

	for _, s := range symbols {
		addFileNode(s.rel)
		provenance := fmt.Sprintf("%s:%d", s.rel, s.def.Line)
		props := map[string]string{
			"origin":     "code_index",
			"kind":       s.def.Kind,
			"references": strconv.Itoa(s.refs),
		}
		if s.def.Receiver != "" {
			props["receiver"] = s.def.Receiver
		}
		symID := "symbol:" + s.def.Name + "@" + s.rel
		_ = kg.AddNode(&core.KGNode{
			ID:         symID,
			Label:      s.def.Name,
			Type:       core.KGNodeSymbol,
			Properties: props,
			Provenance: provenance,
			Confidence: 1.0,
			LastSeen:   now,
		})
		stats.Symbols++
		if err := kg.AddEdge(&core.KGEdge{
			From: "file:" + s.rel, To: symID,
			Relation: core.KGRelDefines, Weight: 1.0,
			Provenance: provenance, CreatedAt: now,
		}); err == nil {
			stats.Edges++
		}
		// Record WHICH files reference the symbol, not just how many. The
		// count alone cannot answer "what breaks if I change this", which is
		// what blast-radius analysis and dead-code detection both need.
		for _, refFile := range s.refFiles {
			addFileNode(refFile)
			if err := kg.AddEdge(&core.KGEdge{
				From: "file:" + refFile, To: symID,
				Relation: core.KGRelReferences, Weight: 1.0,
				Provenance: refFile, CreatedAt: now,
			}); err == nil {
				stats.Edges++
			}
		}
	}

	seenPkgs := make(map[string]bool)
	for rel, imps := range importsByFile {
		addFileNode(rel)
		dedup := make(map[string]bool, len(imps))
		for _, imp := range imps {
			if dedup[imp.Path] {
				continue
			}
			dedup[imp.Path] = true
			pkgID := "package:" + imp.Path
			if !seenPkgs[imp.Path] {
				seenPkgs[imp.Path] = true
				_ = kg.AddNode(&core.KGNode{
					ID:         pkgID,
					Label:      imp.Path,
					Type:       core.KGNodePackage,
					Properties: map[string]string{"origin": "code_index"},
					Confidence: 1.0,
					LastSeen:   now,
				})
				stats.Packages++
			}
			if err := kg.AddEdge(&core.KGEdge{
				From: "file:" + rel, To: pkgID,
				Relation: core.KGRelImports, Weight: 1.0,
				Provenance: rel, CreatedAt: now,
			}); err == nil {
				stats.Edges++
			}
		}
	}

	syncOtherLanguages(ctx, root, kg, relPath, now, addFileNode, &stats)
	return stats, nil
}

// syncOtherLanguages records the same fact shapes for the non-Go source files
// (TypeScript/JavaScript, Python, Rust, Java) using the pattern scanner. The
// node IDs, edge relations and provenance format are identical to the Go pass,
// so a query against the graph does not need to know which language a symbol
// came from — that uniformity is the point.
func syncOtherLanguages(ctx context.Context, root string, kg core.KnowledgeGraphStore,
	relPath func(string) string, now time.Time, addFileNode func(string), stats *SyncStats) {

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			if n := info.Name(); n != "." && (n == "vendor" || n == "node_modules" || n == ".git" || n == "target" || n == "__pycache__") {
				return filepath.SkipDir
			}
			return nil
		}
		lang := intelligence.LanguageOf(path)
		if lang == "" || lang == "go" { // Go is handled exactly, above
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		res := intelligence.ParseText(src, path)
		if len(res.Symbols) == 0 && len(res.Imports) == 0 {
			return nil
		}

		rel := relPath(path)
		addFileNode(rel)

		for _, sym := range res.Symbols {
			provenance := fmt.Sprintf("%s:%d", rel, sym.Line)
			symID := "symbol:" + sym.Name + "@" + rel
			_ = kg.AddNode(&core.KGNode{
				ID:    symID,
				Label: sym.Name,
				Type:  core.KGNodeSymbol,
				Properties: map[string]string{
					"origin": "code_index", "kind": sym.Kind, "language": lang,
				},
				Provenance: provenance,
				// Pattern-scanned symbols are high-confidence but not exact the
				// way go/ast is, and the graph's confidence field exists to
				// carry precisely this distinction.
				Confidence: 0.9,
				LastSeen:   now,
			})
			stats.Symbols++
			if err := kg.AddEdge(&core.KGEdge{
				From: "file:" + rel, To: symID,
				Relation: core.KGRelDefines, Weight: 1.0,
				Provenance: provenance, CreatedAt: now,
			}); err == nil {
				stats.Edges++
			}
		}

		dedup := make(map[string]bool, len(res.Imports))
		for _, imp := range res.Imports {
			if dedup[imp.Path] {
				continue
			}
			dedup[imp.Path] = true
			pkgID := "package:" + imp.Path
			_ = kg.AddNode(&core.KGNode{
				ID: pkgID, Label: imp.Path, Type: core.KGNodePackage,
				Properties: map[string]string{"origin": "code_index", "language": lang},
				Confidence: 0.9, LastSeen: now,
			})
			stats.Packages++
			if err := kg.AddEdge(&core.KGEdge{
				From: "file:" + rel, To: pkgID,
				Relation: core.KGRelImports, Weight: 1.0,
				Provenance: rel, CreatedAt: now,
			}); err == nil {
				stats.Edges++
			}
		}
		return nil
	})
}

// NewKGSyncTool exposes the workspace→KG sync as a deterministic tool so the
// agent (or the user via the tools API) can refresh the code-fact graph on
// demand after large edits. No LLM involved.
func NewKGSyncTool(kg core.KnowledgeGraphStore) *tools.ToolEntry {
	return &tools.ToolEntry{
		Name:          "deterministic_kg_sync",
		Description:   "Re-indexes the workspace's Go code (symbols, imports, reference counts) into the knowledge graph as typed facts with file:line provenance. Run after large refactors so graph answers stay fresh. No LLM involved.",
		Parameters:    tools.MustParseSchema(`{"type":"object","properties":{}}`),
		Deterministic: true,
		Category:      "deterministic",
		Source:        "builtin",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			root := workspaceRoot(ctx)
			stats, err := SyncWorkspaceKG(ctx, root, kg)
			if err != nil {
				return &tools.ToolResult{Name: "deterministic_kg_sync", Success: false, Error: err.Error()}
			}
			return &tools.ToolResult{
				Name: "deterministic_kg_sync", Success: true,
				Output: fmt.Sprintf("Knowledge graph synced from %s: %d files, %d symbols, %d packages, %d edges.",
					root, stats.Files, stats.Symbols, stats.Packages, stats.Edges),
			}
		},
	}
}
