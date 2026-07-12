package deterministic

// definitions.go — deterministic "go to definition" via Go AST. Replaces the
// stub that returned "Definitions found." Scans workspace .go files for
// top-level func/type/struct/interface declarations matching the symbol name.

import (
	"context"
	"fmt"
	"go/token"
	"strings"

	"github.com/darkcode/tools"
)

// NewDefinitionsTool locates the declaration site of a symbol deterministically.
// For Go it uses go/ast (exact structural match); for other languages it falls
// back to ripgrep word-boundary search. No LLM is involved.
func NewDefinitionsTool() *tools.ToolEntry {
	return &tools.ToolEntry{
		Name:          "deterministic_definitions",
		Description:   "Locates the declaration of a symbol deterministically (Go: go/ast for func/type/struct/interface; other languages: ripgrep). Returns file:line and the declaration kind. Use instead of asking the LLM where something is defined.",
		Parameters:    tools.MustParseSchema(definitionsSchema),
		Deterministic: true,
		Category:      "deterministic",
		Source:        "builtin",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			symbol, _ := args["symbol"].(string)
			if strings.TrimSpace(symbol) == "" {
				return &tools.ToolResult{Name: "deterministic_definitions", Success: false, Error: "symbol is required"}
			}
			root := workspaceRoot(ctx)
			files := collectGoFiles(root)
			if len(files) == 0 {
				// Non-Go workspace: fall back to a ripgrep search for the
				// symbol; the first match is the best deterministic guess.
				out, err := ripgrepOrGrep(ctx, root, symbol, "")
				if err != nil || strings.TrimSpace(out) == "" {
					return &tools.ToolResult{Name: "deterministic_definitions", Success: false, Error: "no Go files and no text matches for \"" + symbol + "\""}
				}
				first := strings.SplitN(out, "\n", 2)[0]
				return &tools.ToolResult{
					Name: "deterministic_definitions", Success: true,
					Output: "No Go AST; best text match:\n" + first,
				}
			}
			fset := token.NewFileSet()
			var found []definition
			for _, f := range files {
				for _, d := range parseDefinitions(fset, f) {
					if d.Name == symbol {
						found = append(found, d)
					}
				}
			}
			if len(found) == 0 {
				return &tools.ToolResult{
					Name: "deterministic_definitions", Success: true,
					Output: "No declaration of \"" + symbol + "\" found in " + itoa(len(files)) + " Go file(s) under " + root + ".",
				}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Found %s declaration(s) of %q:\n", itoa(len(found)), symbol)
			for _, d := range found {
				if d.Receiver != "" {
					fmt.Fprintf(&b, "  %s (method on %s) — %s:%d\n", d.Name, d.Receiver, d.File, d.Line)
				} else {
					fmt.Fprintf(&b, "  %s %s — %s:%d\n", d.Kind, d.Name, d.File, d.Line)
				}
			}
			return &tools.ToolResult{Name: "deterministic_definitions", Success: true, Output: b.String()}
		},
	}
}

const definitionsSchema = `{
	"type": "object",
	"properties": {
		"symbol": {"type": "string", "description": "The identifier whose declaration to locate."}
	},
	"required": ["symbol"]
}`
