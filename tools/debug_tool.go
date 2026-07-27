package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/darkcode/debugger"
)

// DebugTool lets the agent read runtime values instead of inferring them.
//
// The alternative loop — add a print, re-run, read stdout, remove the print —
// costs three tool calls, dirties the working tree, and can only observe what
// the agent thought to print. One debugger run observes everything in scope
// and evaluates arbitrary expressions against real state.
type DebugTool struct{ Workspace string }

func (t *DebugTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	fail := func(format string, a ...interface{}) *ToolResult {
		return &ToolResult{Name: "debug", Success: false, Error: fmt.Sprintf(format, a...)}
	}

	file := expandPath(ctx, str(args["file"]))
	if file == "" {
		return fail("file is required — the source file holding the breakpoint")
	}
	line := 0
	if v, ok := args["line"].(float64); ok {
		line = int(v)
	}
	if line <= 0 {
		return fail("line is required and must be a statement, not a blank or comment line")
	}

	// The package to build is the one containing the breakpoint, unless the
	// caller names another.
	dir := expandPath(ctx, str(args["dir"]))
	if dir == "" {
		dir = filepath.Dir(file)
	}

	opts := debugger.Options{Dir: dir, Run: str(args["test_run"]), Program: file}
	// Debugging a test is the common case; a main package is the exception.
	opts.Test = opts.Run != ""
	if v, ok := args["test"].(bool); ok {
		opts.Test = v
	}
	opts.Args = stringList(args["args"])

	report, err := debugger.Inspect(ctx, opts,
		[]debugger.Breakpoint{{File: file, Line: line}},
		stringList(args["expressions"]))
	if err != nil {
		if report != nil && len(report.Unbound) > 0 {
			return fail("%v", err)
		}
		return fail("%v", err)
	}
	if len(report.Observations) == 0 {
		// A clean run that never hit the breakpoint is a real answer, and a
		// more useful one than an error: the code path did not execute.
		return &ToolResult{Name: "debug", Success: true,
			Output: report.Format() + "\nThe breakpoint was never hit — that code path did not run."}
	}
	return &ToolResult{Name: "debug", Success: true, Output: report.Format()}
}

// RegisterDebugTool adds the debugger to the registry.
func RegisterDebugTool(r *Registry, workspace string) {
	t := &DebugTool{Workspace: workspace}
	r.Register(&ToolEntry{
		Name: "debug",
		Description: strings.TrimSpace(`
Run a test or program under the debugger and report the real runtime values at a breakpoint:
every local in scope, any expressions you ask for, and the call stack. Use this instead of adding
print statements — it is one call, it observes everything in scope rather than only what you thought
to print, and it leaves the source untouched.
Set test_run to debug a specific test (the usual case). The line must contain an executable
statement. Go needs delve (go install github.com/go-delve/delve/cmd/dlv@latest); Python needs debugpy
(pip install debugpy). The language is inferred from the file.`),
		Parameters: MustParseSchema(`{
			"type": "object",
			"properties": {
				"file": {"type": "string", "description": "Source file containing the breakpoint"},
				"line": {"type": "integer", "description": "1-based line to stop at; must be an executable statement"},
				"expressions": {"type": "array", "description": "Expressions to evaluate at the breakpoint, e.g. [\"len(items)\", \"cfg.Timeout\"]"},
				"test_run": {"type": "string", "description": "Test name to run, like go test -run. Implies debugging the test binary"},
				"test": {"type": "boolean", "description": "Debug the test binary (default true when test_run is set)"},
				"dir": {"type": "string", "description": "Package directory to build (defaults to the breakpoint file's directory)"},
				"args": {"type": "array", "description": "Arguments passed to the debugged program"}
			},
			"required": ["file", "line"]
		}`),
		Handler:  t.Execute,
		Category: "intelligence",
		// Not read-only: it compiles and executes the project's code, which is
		// the same class of action as the terminal tool.
		ReadOnly: false,
	})
}
