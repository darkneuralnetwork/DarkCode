package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/darkcode/security"
)

// TerminalTool executes shell commands. It supports timeouts and
// captures stdout/stderr separately but merges them in output.
type TerminalTool struct {
	TimeoutSec int
	// Sandbox, when non-nil and available, confines each command so it can
	// only write inside its working directory (the rest of the filesystem is
	// read-only). It is opt-in via DARKCODE_SANDBOX=1 so the default developer
	// workflow is unchanged.
	Sandbox *security.Sandbox
}

func NewTerminalTool() *TerminalTool {
	t := &TerminalTool{TimeoutSec: 120}
	if os.Getenv("DARKCODE_SANDBOX") == "1" {
		if sb := security.NewSandbox(nil); sb.Available() {
			t.Sandbox = sb
		}
	}
	return t
}

func (t *TerminalTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	command, _ := args["command"].(string)
	if command == "" {
		return &ToolResult{Name: "terminal", Success: false, Error: "command is required"}
	}

	timeoutSec, _ := args["timeout"].(float64)
	if timeoutSec == 0 {
		timeoutSec = float64(t.TimeoutSec)
	}

	// Create a sub-context with timeout
	toolCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// Resolve the working directory first. The LLM may pass an explicit
	// "workdir"; otherwise default to the active workspace (the project's path
	// when a project is active) so commands execute in the project the user is
	// working on — matching where write_file/patch land. When no project is
	// active, CurrentWorkspace(ctx) returns "" and exec falls back to the
	// server cwd (the pre-existing behavior).
	workDir := ""
	if workdir, ok := args["workdir"].(string); ok && workdir != "" {
		workDir = workdir
	} else if ws := CurrentWorkspace(ctx); ws != "" {
		workDir = ws
	}

	// Build the argv. When a sandbox is active, the command is confined so it
	// can only write inside workDir; the rest of the filesystem is read-only.
	argv := []string{"bash", "-c", command}
	if t.Sandbox != nil && t.Sandbox.Available() {
		argv = t.Sandbox.Wrap(workDir, argv[0], argv[1:]...)
	}

	cmd := exec.CommandContext(toolCtx, argv[0], argv[1:]...)
	setSysProcAttr(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}

	// Capture stdout and stderr separately
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if workDir != "" {
		cmd.Dir = workDir
	}

	startTime := time.Now()
	err := cmd.Run()
	duration := time.Since(startTime)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if toolCtx.Err() == context.DeadlineExceeded {
			return &ToolResult{
				Name:    "terminal",
				Success: false,
				Error:   fmt.Sprintf("command timed out after %ds", int(timeoutSec)),
				Output:  stdout.String(),
			}
		} else {
			return &ToolResult{
				Name:    "terminal",
				Success: false,
				Error:   err.Error(),
			}
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n" + stderr.String()
	}

	result := &ToolResult{
		Name:    "terminal",
		Success: exitCode == 0,
		Output:  strings.TrimSpace(output),
	}
	if exitCode != 0 {
		result.Error = fmt.Sprintf("exit code %d", exitCode)
	}

	_ = duration // could be used for logging
	return result
}

func (t *TerminalTool) Schema() string {
	return `{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The shell command to execute"
			},
			"workdir": {
				"type": "string",
				"description": "Working directory for the command"
			},
			"timeout": {
				"type": "number",
				"description": "Timeout in seconds (default 120)"
			}
		},
		"required": ["command"]
	}`
}
