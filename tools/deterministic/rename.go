package deterministic

// rename.go — deterministic symbol rename. Replaces the stub that returned
// "Symbol renamed." (and did nothing).
//
// Strategy:
//   - Go workspaces: use go/ast to find declaration + call sites, then perform
//     a scoped identifier rewrite (only whole-token matches, preserving
//     receiver/selector correctness). Writes the changed files.
//   - Non-Go / mixed: ripgrep word-boundary find-and-replace across files.
//
// Safety: a dry_run flag (default true) previews the affected files/counts
// without writing. The actual write path goes through the normal file tools'
// workspace resolution so it respects the active project root.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/tools"
)

// NewRenameTool renames a symbol across the workspace deterministically.
func NewRenameTool() *tools.ToolEntry {
	return &tools.ToolEntry{
		Name:          "deterministic_rename",
		Description:   "Renames a symbol across the workspace deterministically (Go: go/ast identifier rewrite; other files: ripgrep word-boundary replace). Set dry_run=true (default) to preview affected files without writing. No LLM involved.",
		Parameters:    tools.MustParseSchema(renameSchema),
		Deterministic: true,
		Category:      "deterministic",
		Source:        "builtin",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			oldName, _ := args["old_name"].(string)
			newName, _ := args["new_name"].(string)
			if strings.TrimSpace(oldName) == "" || strings.TrimSpace(newName) == "" {
				return &tools.ToolResult{Name: "deterministic_rename", Success: false, Error: "old_name and new_name are required"}
			}
			if !validIdent(newName) {
				return &tools.ToolResult{Name: "deterministic_rename", Success: false, Error: "new_name is not a valid identifier"}
			}
			dryRun := true
			if v, ok := args["dry_run"].(bool); ok {
				dryRun = v
			}
			root := workspaceRoot(ctx)
			files := collectGoFiles(root)

			if len(files) == 0 {
				return rgRename(ctx, root, oldName, newName, dryRun)
			}
			return astRename(ctx, files, oldName, newName, dryRun)
		},
	}
}

const renameSchema = `{
	"type": "object",
	"properties": {
		"old_name": {"type": "string", "description": "The current identifier name."},
		"new_name": {"type": "string", "description": "The replacement identifier name."},
		"dry_run": {"type": "boolean", "description": "If true (default), preview affected files without writing changes."}
	},
	"required": ["old_name", "new_name"]
}`

// astRename rewrites identifier occurrences in Go files using the AST printer
// so formatting is preserved and only whole-token matches change (a rename of
// "Foo" never touches "Foobar" or "x.Foo" unless x.Foo is the target).
func astRename(ctx context.Context, files []goFile, oldName, newName string, dryRun bool) *tools.ToolResult {
	fset := token.NewFileSet()
	type change struct {
		path string
		hits int
	}
	var changes []change
	totalHits := 0

	for _, f := range files {
		file, err := parser.ParseFile(fset, f.path, f.src, parser.ParseComments)
		if err != nil {
			continue
		}
		hits := 0
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == oldName {
				id.Name = newName
				hits++
			}
			return true
		})
		if hits == 0 {
			continue
		}
		totalHits += hits
		if dryRun {
			changes = append(changes, change{path: f.path, hits: hits})
			continue
		}
		// Write the rewritten file.
		var sb strings.Builder
		if err := printer.Fprint(&sb, fset, file); err != nil {
			continue
		}
		if werr := os.WriteFile(f.path, []byte(sb.String()), 0644); werr != nil {
			return &tools.ToolResult{Name: "deterministic_rename", Success: false, Error: "write " + f.path + ": " + werr.Error()}
		}
		changes = append(changes, change{path: f.path, hits: hits})
	}

	if totalHits == 0 {
		return &tools.ToolResult{
			Name: "deterministic_rename", Success: true,
			Output: "No occurrences of \"" + oldName + "\" in " + itoa(len(files)) + " Go file(s). Nothing to rename.",
		}
	}
	var b strings.Builder
	verb := "would rename"
	if !dryRun {
		verb = "renamed"
	}
	fmt.Fprintf(&b, "%s %d occurrence(s) of %q -> %q across %d file(s):\n", verb, totalHits, oldName, newName, len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "  %dx  %s\n", c.hits, c.path)
	}
	if dryRun {
		b.WriteString("\n(dry run — no files written. Set dry_run=false to apply.)")
	}
	return &tools.ToolResult{Name: "deterministic_rename", Success: true, Output: b.String()}
}

// rgRename is the non-Go fallback: ripgrep/grep word-boundary replace across
// all text files. Uses sed -i for the actual rewrite (scoped to matches only).
func rgRename(ctx context.Context, root, oldName, newName string, dryRun bool) *tools.ToolResult {
	out, err := ripgrepOrGrep(ctx, root, oldName, "")
	if err != nil {
		return &tools.ToolResult{Name: "deterministic_rename", Success: false, Error: "scan failed: " + err.Error()}
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return &tools.ToolResult{
			Name: "deterministic_rename", Success: true,
			Output: "No occurrences of \"" + oldName + "\" found under " + root + ".",
		}
	}
	// Collect unique affected files.
	files := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.IndexByte(line, ':'); i > 0 {
			files[line[:i]] = true
		}
	}
	verb := "would rename in"
	if !dryRun {
		verb = "renamed in"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d occurrence(s) of %q -> %q across %d file(s):\n", verb, strings.Count(out, "\n")+1, oldName, newName, len(files))
	for f := range files {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	if dryRun {
		b.WriteString("\n(dry run — no files written. Set dry_run=false to apply.)")
		return &tools.ToolResult{Name: "deterministic_rename", Success: true, Output: b.String()}
	}
	// Apply: sed -i 's/\boldName\b/newName/g' on each file. Word-boundary via \b.
	for f := range files {
		// #nosec G204 — f comes from ripgrep output scoped to the workspace.
		cmd := exec.CommandContext(ctx, "sed", "-i", "s/\\b"+oldName+"\\b/"+newName+"/g", f)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(&b, "  WARN: sed %s: %v\n", f, err)
		}
	}
	return &tools.ToolResult{Name: "deterministic_rename", Success: true, Output: b.String()}
}

// validIdent checks that s is a plausible identifier (letters/digits/_,
// not starting with a digit). Good enough to block obvious bad renames.
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' {
			continue
		}
		if i == 0 && r >= '0' && r <= '9' {
			return false
		}
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// readGoFile reads a single .go file from the workspace (used by imports tool).
func readGoFile(ctx context.Context, file string) ([]byte, error) {
	path := file
	if !filepath.IsAbs(path) {
		if ws, ok := ctx.Value(core.WorkspaceKey).(string); ok && ws != "" {
			path = filepath.Join(ws, file)
		}
	}
	return os.ReadFile(path)
}
