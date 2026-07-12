package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileTool provides file read/write/patch operations.
type FileTool struct{}

func NewFileTool() *FileTool { return &FileTool{} }

// ReadFile reads a file and returns its contents with line numbers.
func (t *FileTool) ReadFile(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return &ToolResult{Name: "read_file", Success: false, Error: "path is required"}
	}

	// Expand ~ to home directory
	path = expandPath(ctx, path)

	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Name: "read_file", Success: false, Error: err.Error()}
	}

	// Add line numbers
	lines := strings.Split(string(data), "\n")
	var result strings.Builder
	for i, line := range lines {
		result.WriteString(fmt.Sprintf("%4d| %s\n", i+1, line))
	}

	return &ToolResult{
		Name:    "read_file",
		Success: true,
		Output:  result.String(),
	}
}

// WriteFile writes content to a file, creating parent directories.
func (t *FileTool) WriteFile(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return &ToolResult{Name: "write_file", Success: false, Error: "path is required"}
	}

	path = expandPath(ctx, path)

	// Create parent directories
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &ToolResult{Name: "write_file", Success: false, Error: err.Error()}
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Name: "write_file", Success: false, Error: err.Error()}
	}

	return &ToolResult{
		Name:    "write_file",
		Success: true,
		Output:  fmt.Sprintf("Wrote %d bytes to %s", len(content), path),
	}
}

// ListDir lists contents of a directory.
func (t *FileTool) ListDir(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return &ToolResult{Name: "list_dir", Success: false, Error: "path is required"}
	}

	path = expandPath(ctx, path)
	entries, err := os.ReadDir(path)
	if err != nil {
		return &ToolResult{Name: "list_dir", Success: false, Error: err.Error()}
	}

	var result strings.Builder
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if entry.IsDir() {
			result.WriteString(fmt.Sprintf("[DIR]  %s\n", entry.Name()))
		} else {
			result.WriteString(fmt.Sprintf("[FILE] %s (%d bytes)\n", entry.Name(), info.Size()))
		}
	}

	if result.Len() == 0 {
		result.WriteString("(empty directory)")
	}

	return &ToolResult{
		Name:    "list_dir",
		Success: true,
		Output:  result.String(),
	}
}

// PatchFile does a targeted find-and-replace in a file.
func (t *FileTool) PatchFile(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	if path == "" || oldStr == "" {
		return &ToolResult{Name: "patch", Success: false, Error: "path and old_string are required"}
	}

	path = expandPath(ctx, path)
	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Name: "patch", Success: false, Error: err.Error()}
	}

	content := string(data)
	if !strings.Contains(content, oldStr) {
		return &ToolResult{Name: "patch", Success: false, Error: "old_string not found in file"}
	}

	replaceAll, _ := args["replace_all"].(bool)
	if replaceAll {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		// Replace first occurrence only
		idx := strings.Index(content, oldStr)
		content = content[:idx] + newStr + content[idx+len(oldStr):]
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Name: "patch", Success: false, Error: err.Error()}
	}

	return &ToolResult{
		Name:    "patch",
		Success: true,
		Output:  fmt.Sprintf("Patched %s", path),
	}
}

// ReplaceFileContent performs a multi-line targeted block replacement.
func (t *FileTool) ReplaceFileContent(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, _ := args["path"].(string)
	oldStr, _ := args["target_content"].(string)
	newStr, _ := args["replacement_content"].(string)
	
	if path == "" || oldStr == "" {
		return &ToolResult{Name: "replace_file_content", Success: false, Error: "path and target_content are required"}
	}

	path = expandPath(ctx, path)
	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Name: "replace_file_content", Success: false, Error: err.Error()}
	}

	content := string(data)
	count := strings.Count(content, oldStr)
	
	if count == 0 {
		return &ToolResult{Name: "replace_file_content", Success: false, Error: "target_content not found in file exactly as provided. Check whitespace/indentation."}
	}
	if count > 1 {
		return &ToolResult{Name: "replace_file_content", Success: false, Error: "target_content found multiple times. Please provide more context to make it unique."}
	}

	content = strings.Replace(content, oldStr, newStr, 1)

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Name: "replace_file_content", Success: false, Error: err.Error()}
	}

	return &ToolResult{
		Name:    "replace_file_content",
		Success: true,
		Output:  fmt.Sprintf("Successfully replaced content in %s", path),
	}
}

// expandPath normalizes a path for a tool call.
//
//  1. ~/…  → expands to the user's home directory.
//  2. absolute paths → returned unchanged.
//  3. relative paths → resolved against the active workspace (the currently
//     active project's path, installed via SetWorkspaceResolver). When no
//     project is active the resolver returns "" and the relative path is
//     returned as-is, so the OS resolves it against the server cwd — the
//     pre-existing behavior. This makes file writes/patches/reads land inside
//     the project the user is working on, matching what the file explorer
//     shows (it already browses ActiveWorkspace).
func expandPath(ctx context.Context, path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return resolveInWorkspace(ctx, path)
}

func (t *FileTool) ReadSchema() string {
	return `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file to read"}
		},
		"required": ["path"]
	}`
}

func (t *FileTool) ListDirSchema() string {
	return `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the directory"}
		},
		"required": ["path"]
	}`
}

func (t *FileTool) WriteSchema() string {
	return `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to write to"},
			"content": {"type": "string", "description": "Content to write"}
		},
		"required": ["path", "content"]
	}`
}

func (t *FileTool) PatchSchema() string {
	return `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file"},
			"old_string": {"type": "string", "description": "Text to find"},
			"new_string": {"type": "string", "description": "Replacement text"},
			"replace_all": {"type": "boolean", "description": "Replace all occurrences"}
		},
		"required": ["path", "old_string", "new_string"]
	}`
}

func (t *FileTool) ReplaceSchema() string {
	return `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file"},
			"target_content": {"type": "string", "description": "Exact text block to find and replace. Must match whitespace exactly."},
			"replacement_content": {"type": "string", "description": "New text block to insert"}
		},
		"required": ["path", "target_content", "replacement_content"]
	}`
}
