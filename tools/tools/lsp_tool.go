package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkcode/tools/intelligence"
)

// LSPTool gives the agent type-aware answers from a language server.
//
// The knowledge graph answers structural questions across the whole repository
// and between sessions. It does not run a type checker, so it cannot say what
// an expression resolves to or what the compiler actually objects to. This
// tool covers exactly that gap, and says so plainly when no language server is
// installed rather than pretending the absence is an answer.
type LSPTool struct {
	Client    *intelligence.LSPClient
	Workspace string
}

func (t *LSPTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	fail := func(format string, a ...interface{}) *ToolResult {
		return &ToolResult{Name: "lsp", Success: false, Error: fmt.Sprintf(format, a...)}
	}
	if t.Client == nil {
		return fail("no language server client configured")
	}

	action, _ := args["action"].(string)
	file := expandPath(ctx, str(args["file"]))
	line, col := 1, 1
	if v, ok := args["line"].(float64); ok && v > 0 {
		line = int(v)
	}
	if v, ok := args["column"].(float64); ok && v > 0 {
		col = int(v)
	}

	needFile := func() *ToolResult {
		if file == "" {
			return fail("%s requires file", action)
		}
		return nil
	}

	var (
		result   interface{}
		headline string
		err      error
	)
	switch action {
	case "definition":
		if bad := needFile(); bad != nil {
			return bad
		}
		var defFile string
		var defLine int
		defFile, defLine, err = t.Client.Definition(file, line, col)
		if err == nil {
			result = intelligence.Location{File: defFile, Line: defLine}
			headline = fmt.Sprintf("declared at %s:%d", defFile, defLine)
		}

	case "references":
		if bad := needFile(); bad != nil {
			return bad
		}
		var locs []intelligence.Location
		locs, err = t.Client.References(file, line, col)
		result = locs
		headline = fmt.Sprintf("%d reference(s)", len(locs))

	case "hover":
		if bad := needFile(); bad != nil {
			return bad
		}
		var text string
		text, err = t.Client.Hover(file, line, col)
		result = text
		headline = "resolved type and documentation"

	case "diagnostics":
		if bad := needFile(); bad != nil {
			return bad
		}
		var diags []intelligence.Diagnostic
		diags, err = t.Client.Diagnostics(file)
		result = diags
		if len(diags) == 0 {
			headline = "no problems reported by the language server"
		} else {
			headline = fmt.Sprintf("%d problem(s) reported by the language server", len(diags))
		}

	case "symbol":
		query := str(args["query"])
		if query == "" {
			return fail("symbol requires query")
		}
		// workspace/symbol is repository-wide, but a server still has to be
		// picked; any file of the right language identifies which one.
		if file == "" {
			file = t.Workspace + "/main.go"
		}
		var locs []intelligence.Location
		locs, err = t.Client.WorkspaceSymbol(query, file)
		result = locs
		headline = fmt.Sprintf("%d symbol(s) matching %q", len(locs), query)

	default:
		return fail("unknown action %q (want: definition, references, hover, diagnostics, symbol)", action)
	}

	if err != nil {
		// A missing language server is expected, not a failure of the agent's
		// reasoning — say which it is so the model falls back rather than retries.
		return fail("%v — fall back to graph_query or search_files for this one", err)
	}

	body, mErr := json.MarshalIndent(result, "", "  ")
	if mErr != nil {
		return fail("%v", mErr)
	}
	return &ToolResult{Name: "lsp", Success: true, Output: headline + "\n" + string(body)}
}

// RegisterLSPTool adds the language-server tool to the registry.
func RegisterLSPTool(r *Registry, workspace string) *intelligence.LSPClient {
	client := intelligence.NewLSPClient(workspace)
	t := &LSPTool{Client: client, Workspace: workspace}
	r.Register(&ToolEntry{
		Name: "lsp",
		Description: strings.TrimSpace(`
Ask a language server type-aware questions the code graph cannot answer: what a symbol actually
resolves to, its real signature and docs, and what the compiler or type checker objects to.
Positions are 1-based line and column.
Actions: definition (where a symbol is declared), references (every use), hover (resolved type and
documentation), diagnostics (real type/compile errors in a file), symbol (repository-wide search).
Requires the language's server on PATH (gopls, typescript-language-server, pyright, rust-analyzer,
jdtls); when absent this reports so and you should use graph_query or search_files instead.`),
		Parameters: MustParseSchema(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["definition", "references", "hover", "diagnostics", "symbol"], "description": "Which query to run"},
				"file": {"type": "string", "description": "Path to the file"},
				"line": {"type": "integer", "description": "1-based line number"},
				"column": {"type": "integer", "description": "1-based column number"},
				"query": {"type": "string", "description": "Symbol name for the symbol action"}
			},
			"required": ["action"]
		}`),
		Handler:  t.Execute,
		Category: "intelligence",
		// Querying a language server mutates nothing, so Chat mode can use it.
		ReadOnly:      true,
		Deterministic: true,
	})
	return client
}
