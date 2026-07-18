package deterministic

// kgsync.go — feeds the deterministic AST index into the knowledge graph
// (local-first upgrade plan §5/§7 Phase B).
//
// The definitions/imports/dependencies tools already compute high-quality
// structured facts per call and throw them away. SyncWorkspaceKG records those
// same facts as typed, provenance-carrying nodes/edges so the graph can answer
// structural questions ("where is X defined", "which files import Y") by
// traversal with citations — no LLM. Every node carries Provenance
// ("file.go:line") and LastSeen, which is what makes a graph answer
// high-confidence by construction: it points at real code.
//
// Node/edge scheme (IDs reuse the existing "file:<path>" convention from
// orchestrator/memory_recorder.go so activity facts and code facts connect):
//
//	file:<relpath>            ── defines ──▶  symbol:<name>@<relpath>
//	file:<relpath>            ── imports ──▶  package:<importpath>
//
// Reference fan-in is stored as a `references` count property on the symbol
// node (how many OTHER files mention the identifier) rather than one edge per
// referencing file — full per-site citations remain the job of the live
// deterministic_references tool (cascade rung 0); the graph answers the
// aggregate ("referenced by 6 files") cheaply.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/tools"
)

// SyncStats summarizes one workspace→KG sync.
type SyncStats struct {
	Files    int `json:"files"`
	Symbols  int `json:"symbols"`
	Packages int `json:"packages"`
	Edges    int `json:"edges"`
}

// SyncWorkspaceKG scans the Go files under root and records their symbols,
// imports, and reference counts in the knowledge graph as typed facts with
// provenance. It is idempotent: nodes are upserted by ID and the KG's AddEdge
// reinforces rather than duplicates, so periodic re-syncs keep the graph
// fresh without growing it. Non-Go workspaces are a no-op (Stats zero).
func SyncWorkspaceKG(ctx context.Context, root string, kg core.KnowledgeGraphStore) (SyncStats, error) {
	var stats SyncStats
	if kg == nil {
		return stats, fmt.Errorf("nil knowledge graph")
	}
	files := collectGoFiles(root)
	if len(files) == 0 {
		return stats, nil
	}
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
		def  definition
		rel  string // defining file, relative
		refs int    // number of OTHER files mentioning the identifier
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
			}
		}
	}

	// Write facts. File nodes first so edges always resolve.
	seenFiles := make(map[string]bool)
	addFileNode := func(rel string) {
		if seenFiles[rel] {
			return
		}
		seenFiles[rel] = true
		_ = kg.AddNode(&core.KGNode{
			ID:         "file:" + rel,
			Label:      rel,
			Type:       core.KGNodeFile,
			Properties: map[string]string{"origin": "code_index"},
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
			"references": itoa(s.refs),
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

	return stats, nil
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
