package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/darkcode/security"
)

// TerminalTool executes shell commands. It supports timeouts and
// captures stdout/stderr separately but merges them in output.
type TerminalTool struct {
	TimeoutSec int
	// Sandbox, when non-nil and active, confines each command so it can only
	// write inside its working directory (plus configured cache dirs); the rest
	// of the filesystem is read-only. Injected at startup from config so there
	// is exactly one sandbox for the process. nil means no confinement.
	Sandbox *security.Sandbox
	// Backend decides where a command runs (local, Docker, SSH). nil means
	// local, sandboxed — the default.
	Backend Backend
}

// NewTerminalTool builds the terminal tool with the process sandbox (may be nil).
func NewTerminalTool(sb *security.Sandbox) *TerminalTool {
	return &TerminalTool{TimeoutSec: 120, Sandbox: sb, Backend: LocalBackend{Sandbox: sb}}
}

func (t *TerminalTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	command, _ := args["command"].(string)
	if command == "" {
		return &ToolResult{Name: "terminal", Success: false, Error: "command is required"}
	}

	// The sandbox confines local execution only; Docker and SSH provide their
	// own isolation, so a strict-mode refusal would be wrong there.
	local := t.Backend == nil || t.Backend.Name() == "local"

	// A backend that could not be built refuses every command. Running locally
	// instead would silently contradict what the user configured.
	if mb, bad := t.Backend.(MisconfiguredBackend); bad {
		return &ToolResult{Name: "terminal", Success: false,
			Error: "blocked: the configured execution backend is unusable — " + mb.Err.Error() +
				". Commands are NOT running locally as a fallback; fix execution_backend or set it to \"local\"."}
	}

	// Strict sandbox with no backend fails closed rather than running unconfined.
	if local && t.Sandbox != nil && t.Sandbox.MustRefuse() {
		return &ToolResult{Name: "terminal", Success: false,
			Error: "blocked: sandbox mode is 'strict' but no sandbox backend (bwrap/firejail) is installed — install one, or set sandbox to 'auto'/'on'/'off'"}
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

	// Build the argv via the configured backend. Locally that wraps the command
	// in the filesystem sandbox; Docker and SSH move execution off this machine
	// entirely.
	backend := t.Backend
	if backend == nil {
		backend = LocalBackend{Sandbox: t.Sandbox}
	}
	argv := backend.Argv(workDir, command)

	cmd := exec.CommandContext(toolCtx, argv[0], argv[1:]...)
	setSysProcAttr(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}

	// Capture stdout and stderr separately
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// The host-side working directory only means something for local runs; the
	// other backends carry the directory inside their own argv.
	if workDir != "" && local {
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
