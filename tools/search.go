package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/internal/repowalk"
)

// SearchTool searches file contents using ripgrep (rg) or falls back to grep.
type SearchTool struct{}

func NewSearchTool() *SearchTool { return &SearchTool{} }

// SearchContent searches inside files for a pattern.
func (t *SearchTool) SearchContent(ctx context.Context, args map[string]interface{}) *ToolResult {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return &ToolResult{Name: "search_files", Success: false, Error: "pattern is required"}
	}

	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	path = expandPath(ctx, path)

	// Try ripgrep first, fall back to grep
	// argv, not a shell string, so the pattern cannot inject — but rg still has
	// to be told to leave the agent's own state and the usual noise alone.
	cmd := exec.CommandContext(ctx, "rg", "--line-number", "--no-heading", "--color=never",
		"--glob", "!.darkcode", "--glob", "!node_modules", "--glob", "!vendor", "--glob", "!.git",
		pattern, path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if rg is not installed
		if strings.Contains(err.Error(), "executable file not found") {
			// Fall back to grep -rn
			cmd = exec.CommandContext(ctx, "grep", "-rn", "--color=never",
				"--exclude-dir=.darkcode", "--exclude-dir=node_modules",
				"--exclude-dir=vendor", "--exclude-dir=.git", pattern, path)
			output, err = cmd.CombinedOutput()
			if err != nil && len(output) == 0 {
				return &ToolResult{Name: "search_files", Success: false, Error: "no matches or error: " + err.Error()}
			}
		} else if len(output) == 0 {
			return &ToolResult{Name: "search_files", Success: false, Error: fmt.Sprintf("search failed: %v", err)}
		}
	}

	// Limit output to prevent context overflow
	result := string(output)
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}

	return &ToolResult{
		Name:    "search_files",
		Success: true,
		Output:  strings.TrimSpace(result),
	}
}

// ListFiles lists files matching a glob pattern, sorted by modification time.
// listFilesMax bounds the reply. A listing longer than this is not read; it is
// skimmed, and the model should narrow the pattern instead.
const listFilesMax = 200

// listFilesMaxDepth matches the previous behaviour: deep enough to see a
// project's shape, shallow enough not to walk a monorepo.
const listFilesMaxDepth = 3

// ListFiles lists files under path matching a glob, newest first.
//
// This was a `bash -c` string with the model-supplied path and pattern
// interpolated into it, so a path of `. ; touch /tmp/x ; echo ` ran that
// command — verified before the rewrite. It also skipped nothing, which is why
// asking what was in a workspace listed the agent's own memory stores back to
// it.
//
// Walking in Go fixes both and drops the dependency on bash, find, sort, head
// and cut — none of which exist on Windows, where this tool silently never
// worked.
func (t *SearchTool) ListFiles(ctx context.Context, args map[string]interface{}) *ToolResult {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		pattern = "*"
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return &ToolResult{Name: "list_files", Success: false, Error: "invalid pattern: " + err.Error()}
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	root := expandPath(ctx, path)

	type entry struct {
		path string
		mod  time.Time
	}
	var found []entry

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not a failed listing
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && repowalk.SkipDir(d.Name()) {
				return fs.SkipDir
			}
			if depth(root, p) >= listFilesMaxDepth {
				return fs.SkipDir
			}
			return nil
		}
		if ok, _ := filepath.Match(pattern, d.Name()); !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		found = append(found, entry{path: p, mod: info.ModTime()})
		return nil
	})
	if err != nil && len(found) == 0 {
		return &ToolResult{Name: "list_files", Success: false, Error: err.Error()}
	}

	// Newest first, then by path so equal timestamps do not reorder between
	// identical calls — a listing that shuffles defeats the answer cache.
	sort.Slice(found, func(i, j int) bool {
		if !found[i].mod.Equal(found[j].mod) {
			return found[i].mod.After(found[j].mod)
		}
		return found[i].path < found[j].path
	})

	truncated := false
	if len(found) > listFilesMax {
		found, truncated = found[:listFilesMax], true
	}

	var b strings.Builder
	for _, e := range found {
		b.WriteString(e.path)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "... more than %d matches; narrow the pattern\n", listFilesMax)
	}
	if b.Len() == 0 {
		return &ToolResult{Name: "list_files", Success: true, Output: "no files matched " + pattern}
	}
	return &ToolResult{Name: "list_files", Success: true, Output: strings.TrimRight(b.String(), "\n")}
}

// depth returns how many directories p is below root.
func depth(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

func (t *SearchTool) SearchSchema() string {
	return `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Regex pattern to search for"},
			"path": {"type": "string", "description": "Directory or file to search in (default: current dir)"}
		},
		"required": ["pattern"]
	}`
}

func (t *SearchTool) ListSchema() string {
	return `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern (e.g. *.py)"},
			"path": {"type": "string", "description": "Directory to search in (default: current dir)"}
		}
	}`
}
