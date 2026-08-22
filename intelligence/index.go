package intelligence

import (
	"context"
	"github.com/darkcode/internal/repowalk"
	"os"
	"path/filepath"
)

// WorkspaceMetadata holds generic info about the project workspace.
type WorkspaceMetadata struct {
	RootPath string
	Language string // primary language detected ("go", "js", "mixed", …)
}

// ProjectIndex provides deterministic code understanding via AST parsing.
type ProjectIndex struct {
	symbols   *SymbolGraph
	deps      *DependencyGraph
	calls     *CallGraph
	imports   *ImportGraph
	classes   *ClassGraph
	workspace *WorkspaceMetadata
	lsp       LSPBridge
	watcher   *FileWatcher
}

// NewProjectIndex creates a project index rooted at rootPath.
func NewProjectIndex(rootPath string) *ProjectIndex {
	return &ProjectIndex{
		symbols:   NewSymbolGraph(),
		deps:      NewDependencyGraph(),
		calls:     NewCallGraph(),
		imports:   NewImportGraph(),
		classes:   NewClassGraph(),
		workspace: &WorkspaceMetadata{RootPath: rootPath, Language: "go"},
		lsp:       NewLSPBridge(rootPath),
		watcher:   NewFileWatcher(rootPath, 0), // default interval
	}
}

// Query returns symbols matching a deterministic query (no LLM).
func (p *ProjectIndex) Query(q SymbolQuery) ([]Symbol, error) {
	return p.symbols.Find(q), nil
}

// ScanWorkspace walks the root, parses every .go file, and populates every
// graph. This is the bulk-index path; incremental updates use the FileWatcher.
func (p *ProjectIndex) ScanWorkspace() error {
	p.symbols.Clear()
	// reset the mutable graphs
	p.deps = NewDependencyGraph()
	p.calls = NewCallGraph()
	p.imports = NewImportGraph()
	p.classes = NewClassGraph()

	parser := NewASTParser()
	goFiles := 0
	otherFiles := 0
	_ = filepath.Walk(p.workspace.RootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		lang := LanguageOf(path)
		if lang == "" || repowalk.SkipPath(path) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if lang == "go" {
			goFiles++
			res, perr := parser.Parse(data, path)
			if perr != nil {
				return nil
			}
			p.ingest(path, res)
			return nil
		}
		otherFiles++
		p.ingest(path, ParseText(data, path))
		return nil
	})
	if goFiles == 0 && otherFiles > 0 {
		p.workspace.Language = detectLanguage(p.workspace.RootPath)
	} else if goFiles > 0 && otherFiles > 0 {
		p.workspace.Language = "mixed"
	}
	return nil
}

// Reindex re-parses a single file into the graphs. It is the incremental half
// ScanWorkspace's comment always promised and nothing ever called: the watcher
// exists to avoid re-walking the tree on every change, but without a per-file
// path the only way to apply a change was a full rescan, so the watcher was
// built, wired to nothing, and left running nowhere.
//
// A deleted file is ignored rather than treated as an error: the symbols it
// contributed go stale until the next full scan, which is a smaller wrong
// answer than refusing to track any change at all.
func (p *ProjectIndex) Reindex(path string) {
	lang := LanguageOf(path)
	if lang == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if lang == "go" {
		res, perr := NewASTParser().Parse(data, path)
		if perr != nil {
			return
		}
		p.ingest(path, res)
		return
	}
	p.ingest(path, ParseText(data, path))
}

// StartWatching keeps the index fresh in the background: the watcher polls for
// mtime changes and each changed file is re-parsed in place. Cancel via ctx or
// StopWatching.
//
// Callers must hold the index for as long as they want it watched. A
// request-scoped index must not call this — the goroutine would outlive the
// request and keep re-parsing an index nobody can read.
func (p *ProjectIndex) StartWatching(ctx context.Context) {
	if p.watcher == nil {
		return
	}
	p.watcher.OnChange = func(changed []string) {
		for _, f := range changed {
			p.Reindex(f)
		}
	}
	p.watcher.Start(ctx)
}

// StopWatching halts background indexing.
func (p *ProjectIndex) StopWatching() {
	if p.watcher != nil {
		p.watcher.Stop()
	}
}

// ingest merges one file's parse result into every graph.
func (p *ProjectIndex) ingest(file string, res ParseResult) {
	// symbols
	for _, sym := range res.Symbols {
		p.symbols.Add(sym)
		if sym.Kind == "struct" || sym.Kind == "interface" || sym.Kind == "type" {
			p.classes.AddType(sym.Name, sym.Kind)
		}
	}
	// imports + dependency edges (file -> pkg, and pkg(pkgOf(file)) -> pkg)
	pkg := packageOf(file)
	for _, imp := range res.Imports {
		p.imports.Add(file, imp.Path)
		p.deps.AddEdge(pkg, imp.Path)
	}
	// call edges
	for _, c := range res.Calls {
		p.calls.Add(c)
	}
	// class embeddings
	for _, e := range res.Embeds {
		p.classes.AddEmbed(e.Type, e.Embedded)
	}
}

// Stats returns a summary of the index (for the UI / diagnostics).
func (p *ProjectIndex) Stats() map[string]interface{} {
	funcCount, typeCount := 0, 0
	for _, s := range p.symbols.All() {
		if s.Kind == "function" {
			funcCount++
		} else {
			typeCount++
		}
	}
	return map[string]interface{}{
		"total_symbols": p.symbols.Count(),
		"functions":     funcCount,
		"types":         typeCount,
		"packages":      p.deps.PackageCount(),
		"indexed_files": p.imports.FileCount(),
		"call_edges":    p.calls.EdgeCount(),
		"class_types":   p.classes.TypeCount(),
		"language":      p.workspace.Language,
		"lsp_connected": p.lsp.Available(),
		"health":        "Online",
	}
}

// Graphs exposes the individual graphs for tool/UI consumption.
func (p *ProjectIndex) Symbols() *SymbolGraph          { return p.symbols }
func (p *ProjectIndex) Dependencies() *DependencyGraph { return p.deps }
func (p *ProjectIndex) Calls() *CallGraph              { return p.calls }
func (p *ProjectIndex) Imports() *ImportGraph          { return p.imports }
func (p *ProjectIndex) Classes() *ClassGraph           { return p.classes }
func (p *ProjectIndex) Workspace() *WorkspaceMetadata  { return p.workspace }
func (p *ProjectIndex) Watcher() *FileWatcher          { return p.watcher }

// packageOf derives the Go package path from a file path (directory).
func packageOf(file string) string {
	return filepath.Dir(file)
}

// detectLanguage guesses the primary language from marker files.
func detectLanguage(root string) string {
	for _, m := range []struct{ file, lang string }{
		{"go.mod", "go"},
		{"package.json", "js"},
		{"Cargo.toml", "rust"},
		{"pom.xml", "java"},
		{"setup.py", "python"},
		{"pyproject.toml", "python"},
	} {
		if _, err := os.Stat(filepath.Join(root, m.file)); err == nil {
			return m.lang
		}
	}
	return "unknown"
}
