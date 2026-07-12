package deterministic

// dependencies.go — deterministic dependency analysis. Replaces the stub that
// returned "Dependencies found." Builds a file→import-paths and
// package→packages map from Go AST.

import (
	"context"
	"fmt"
	"go/token"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkcode/tools"
)

// NewDependenciesTool builds the workspace dependency graph deterministically.
// Output: per-file import list + a reverse index (which files import a given
// package). No LLM.
func NewDependenciesTool() *tools.ToolEntry {
	return &tools.ToolEntry{
		Name:          "deterministic_dependencies",
		Description:   "Builds a Go dependency map deterministically (go/ast): per-file imports and a reverse index of which files import each package. Use to answer 'what depends on X?' without an LLM.",
		Parameters:    tools.MustParseSchema(dependenciesSchema),
		Deterministic: true,
		Category:      "deterministic",
		Source:        "builtin",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			root := workspaceRoot(ctx)
			files := collectGoFiles(root)
			if len(files) == 0 {
				return &tools.ToolResult{Name: "deterministic_dependencies", Success: false, Error: "no .go files under " + root}
			}
			fset := token.NewFileSet()

			// forward: file -> []path ; reverse: path -> []file
			forward := map[string][]string{}
			reverse := map[string][]string{}
			for _, f := range files {
				imports := parseImports(fset, f)
				seen := map[string]bool{}
				for _, imp := range imports {
					if seen[imp.Path] {
						continue
					}
					seen[imp.Path] = true
					forward[f.path] = append(forward[f.path], imp.Path)
					reverse[imp.Path] = append(reverse[imp.Path], f.path)
				}
			}

			// If a specific package is requested, return its reverse deps.
			if pkg, _ := args["package"].(string); pkg != "" {
				files := reverse[pkg]
				if len(files) == 0 {
					return &tools.ToolResult{
						Name: "deterministic_dependencies", Success: true,
						Output: "No files import \"" + pkg + "\".",
					}
				}
				sort.Strings(files)
				var b strings.Builder
				fmt.Fprintf(&b, "%d file(s) import %s:\n", len(files), pkg)
				for _, fp := range files {
					fmt.Fprintf(&b, "  %s\n", fp)
				}
				return &tools.ToolResult{Name: "deterministic_dependencies", Success: true, Output: b.String()}
			}

			var b strings.Builder
			fmt.Fprintf(&b, "Dependency graph for %s (%d Go files):\n\n", root, len(files))

			// Forward index, sorted by path.
			paths := make([]string, 0, len(forward))
			for p := range forward {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				rels := forward[p]
				sort.Strings(rels)
				fmt.Fprintf(&b, "%s ->\n", p)
				for _, r := range rels {
					fmt.Fprintf(&b, "    %s\n", r)
				}
			}

			// Top imported packages (hub analysis).
			type kv struct {
				pkg string
				n   int
			}
			var hubs []kv
			for p, fs := range reverse {
				hubs = append(hubs, kv{p, len(fs)})
			}
			sort.Slice(hubs, func(i, j int) bool { return hubs[i].n > hubs[j].n })
			fmt.Fprintf(&b, "\nMost-imported packages:\n")
			for i, h := range hubs {
				if i >= 15 {
					break
				}
				fmt.Fprintf(&b, "  %dx  %s\n", h.n, h.pkg)
			}
			_ = filepath.Separator // keep import for future path-relativization
			return &tools.ToolResult{Name: "deterministic_dependencies", Success: true, Output: truncateOutput(b.String())}
		},
	}
}

const dependenciesSchema = `{
	"type": "object",
	"properties": {
		"package": {"type": "string", "description": "Optional: return only the files that import this package (reverse dependency lookup)."}
	}
}`
