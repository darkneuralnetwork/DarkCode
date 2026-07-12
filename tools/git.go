package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GitTool provides git operations as a tool.
type GitTool struct{}

// NewGitTool creates a new git tool.
func NewGitTool() *GitTool {
	return &GitTool{}
}

// Schema returns the JSON Schema for the git tool.
func (g *GitTool) Schema() string {
	return `{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"description": "Git action: status, diff, log, branch, add, commit, stash, show",
				"enum": ["status", "diff", "log", "branch", "add", "commit", "stash", "show"]
			},
			"args": {
				"type": "string",
				"description": "Additional arguments for the git command (e.g. file paths, commit message, branch name)"
			},
			"path": {
				"type": "string",
				"description": "Working directory path (defaults to current directory)"
			}
		},
		"required": ["action"]
	}`
}

// Execute runs the git command.
func (g *GitTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	action, _ := args["action"].(string)
	extraArgs, _ := args["args"].(string)
	workDir, _ := args["path"].(string)
	if workDir != "" {
		// Resolve ~/ and relative paths the same way file tools do, so a
		// relative "path" arg lands inside the active project.
		workDir = expandPath(ctx, workDir)
	}
	if workDir == "" {
		// No explicit path → default to the active workspace (the project's
		// path when a project is active) so git operates on the project the
		// user is working on. Falls back to "." (server cwd) when no project
		// is active — the pre-existing behavior.
		if ws := CurrentWorkspace(ctx); ws != "" {
			workDir = ws
		} else {
			workDir = "."
		}
	}

	if action == "" {
		return &ToolResult{Name: "git", Success: false, Error: "action is required"}
	}

	var cmdArgs []string
	switch action {
	case "status":
		cmdArgs = []string{"status", "--short", "--branch"}
	case "diff":
		cmdArgs = []string{"diff"}
		if extraArgs != "" {
			cmdArgs = append(cmdArgs, strings.Fields(extraArgs)...)
		} else {
			cmdArgs = append(cmdArgs, "--stat")
		}
	case "log":
		cmdArgs = []string{"log", "--oneline", "-20"}
		if extraArgs != "" {
			cmdArgs = append(cmdArgs, strings.Fields(extraArgs)...)
		}
	case "branch":
		cmdArgs = []string{"branch", "-a"}
		if extraArgs != "" {
			cmdArgs = append(cmdArgs, strings.Fields(extraArgs)...)
		}
	case "add":
		if extraArgs == "" {
			extraArgs = "."
		}
		cmdArgs = append([]string{"add"}, strings.Fields(extraArgs)...)
	case "commit":
		if extraArgs == "" {
			return &ToolResult{Name: "git", Success: false, Error: "commit message required (pass in 'args')"}
		}
		cmdArgs = []string{"commit", "-m", extraArgs}
	case "stash":
		cmdArgs = []string{"stash"}
		if extraArgs != "" {
			cmdArgs = append(cmdArgs, strings.Fields(extraArgs)...)
		}
	case "show":
		cmdArgs = []string{"show", "--stat"}
		if extraArgs != "" {
			cmdArgs = append(cmdArgs, strings.Fields(extraArgs)...)
		}
	default:
		return &ToolResult{Name: "git", Success: false, Error: fmt.Sprintf("unknown action: %s", action)}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "git", cmdArgs...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err != nil {
		return &ToolResult{
			Name:    "git",
			Success: false,
			Output:  output,
			Error:   err.Error(),
		}
	}

	if len(output) > 50000 {
		output = output[:50000] + "\n... (truncated)"
	}

	return &ToolResult{
		Name:    "git",
		Success: true,
		Output:  output,
	}
}
