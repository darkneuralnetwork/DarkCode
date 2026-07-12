package deterministic

// imports.go — deterministic import listing/resolution via Go AST. Replaces
// the stub that returned "Imports found."

import (
	"context"
	"fmt"
	"go/token"
	"strings"

	"github.com/darkcode/tools"
)

// NewImportsTool lists the imports of a Go file (or every .go file in the
// workspace when no file is given) deterministically. No LLM.
func NewImportsTool() *tools.ToolEntry {
	return &tools.ToolEntry{
		Name:          "deterministic_imports",
		Description:   "Lists Go import declarations deterministically (go/ast). Pass 'file' for one file's imports, or omit to list imports across the whole workspace. Use to inspect dependencies without an LLM.",
		Parameters:    tools.MustParseSchema(importsSchema),
		Deterministic: true,
		Category:      "deterministic",
		Source:        "builtin",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			root := workspaceRoot(ctx)
			file, _ := args["file"].(string)
			fset := token.NewFileSet()

			var files []goFile
			if file != "" {
				data, err := readGoFile(ctx, file)
				if err != nil {
					return &tools.ToolResult{Name: "deterministic_imports", Success: false, Error: err.Error()}
				}
				files = []goFile{{path: file, src: data}}
			} else {
				files = collectGoFiles(root)
				if len(files) == 0 {
					return &tools.ToolResult{Name: "deterministic_imports", Success: false, Error: "no .go files under " + root}
				}
			}

			var b strings.Builder
			total := 0
			for _, f := range files {
				imports := parseImports(fset, f)
				if len(imports) == 0 {
					continue
				}
				fmt.Fprintf(&b, "%s (%d):\n", f.path, len(imports))
				for _, imp := range imports {
					total++
					if imp.Alias != "" {
						fmt.Fprintf(&b, "  %s as %s\n", imp.Path, imp.Alias)
					} else {
						fmt.Fprintf(&b, "  %s\n", imp.Path)
					}
				}
			}
			fmt.Fprintf(&b, "\n%d import(s) across %d file(s).\n", total, len(files))
			return &tools.ToolResult{Name: "deterministic_imports", Success: true, Output: truncateOutput(b.String())}
		},
	}
}

const importsSchema = `{
	"type": "object",
	"properties": {
		"file": {"type": "string", "description": "Optional: a single .go file to inspect. Omit to list imports across the whole workspace."}
	}
}`
