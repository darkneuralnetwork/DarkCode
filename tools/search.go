package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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
	cmd := exec.CommandContext(ctx, "rg", "--line-number", "--no-heading", "--color=never", pattern, path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if rg is not installed
		if strings.Contains(err.Error(), "executable file not found") {
			// Fall back to grep -rn
			cmd = exec.CommandContext(ctx, "grep", "-rn", "--color=never", pattern, path)
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
func (t *SearchTool) ListFiles(ctx context.Context, args map[string]interface{}) *ToolResult {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		pattern = "*"
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	path = expandPath(ctx, path)

	// Use find with sort by modification time
	cmd := exec.CommandContext(ctx, "bash", "-c",
		fmt.Sprintf("find %s -maxdepth 3 -name '%s' -type f -printf '%%T@ %%p\\n' 2>/dev/null | sort -rn | head -50 | cut -d' ' -f2-", path, pattern))
	output, err := cmd.CombinedOutput()
	if err != nil && len(output) == 0 {
		return &ToolResult{Name: "list_files", Success: false, Error: err.Error()}
	}

	return &ToolResult{
		Name:    "list_files",
		Success: true,
		Output:  strings.TrimSpace(string(output)),
	}
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
