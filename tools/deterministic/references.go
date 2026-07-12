package deterministic

// references.go — deterministic "find references" via ripgrep word-boundary
// search. Replaces the stub that returned the literal "References found."

import (
	"context"
	"strings"

	"github.com/darkcode/tools"
)

// NewReferencesTool finds every occurrence of a symbol across the workspace
// using ripgrep (grep fallback), with word-boundary matching so a search for
// "Foo" does not match "Foobar" or "myFoo". This is the deterministic
// alternative to asking an LLM "where is X used?".
func NewReferencesTool() *tools.ToolEntry {
	return &tools.ToolEntry{
		Name:          "deterministic_references",
		Description:   "Finds all references to a symbol across the workspace deterministically (ripgrep word-boundary match, no LLM). Returns file:line:column matches. Use for find-references before any rename.",
		Parameters:    tools.MustParseSchema(referencesSchema),
		Deterministic: true,
		Category:      "deterministic",
		Source:        "builtin",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			symbol, _ := args["symbol"].(string)
			if strings.TrimSpace(symbol) == "" {
				return &tools.ToolResult{Name: "deterministic_references", Success: false, Error: "symbol is required"}
			}
			glob, _ := args["glob"].(string)
			root := workspaceRoot(ctx)
			out, err := ripgrepOrGrep(ctx, root, symbol, glob)
			if err != nil {
				return &tools.ToolResult{Name: "deterministic_references", Success: false, Error: "search failed: " + err.Error()}
			}
			out = strings.TrimSpace(out)
			if out == "" {
				return &tools.ToolResult{
					Name: "deterministic_references", Success: true,
					Output: "No references to \"" + symbol + "\" found under " + root + ".",
				}
			}
			count := strings.Count(out, "\n") + 1
			return &tools.ToolResult{
				Name: "deterministic_references", Success: true,
				Output: strings.TrimSpace(truncateOutput(out)) + "\n\n---\n" + itoa(count) + " reference(s) across the workspace.",
			}
		},
	}
}

const referencesSchema = `{
	"type": "object",
	"properties": {
		"symbol": {"type": "string", "description": "The identifier to find references for (word-boundary matched)."},
		"glob": {"type": "string", "description": "Optional file glob to restrict the search, e.g. '*.go' or 'src/**/*.ts'."}
	},
	"required": ["symbol"]
}`
