package main

import (
	"context"
	"fmt"
	"os"

	"github.com/darkcode/acp"
	"github.com/darkcode/core"
	"github.com/darkcode/permission"
)

// acpExecutor adapts the orchestration kernel to the ACP agent's Executor
// interface, scoping each turn to the session's working directory.
type acpExecutor struct{ runner *AppRunner }

func (e acpExecutor) Execute(ctx context.Context, cwd, prompt string) (string, error) {
	if cwd != "" {
		ctx = context.WithValue(ctx, core.WorkspaceKey, cwd)
	}
	return e.runner.Kernel.Execute(ctx, prompt)
}

// RunACP serves the Agent Client Protocol on stdio until the editor
// disconnects.
//
// stdout is the protocol channel, so nothing else may write to it — a stray
// log line is a malformed message that desynchronises the stream. Logs go to
// stderr, which editors surface separately.
func (a *AppRunner) RunACP() {
	defer a.gracefulShutdown()

	// The editor owns the conversation surface and cannot render our approval
	// prompts, so an unattended agent must not block on one. ACP's own
	// permission flow is the right long-term answer; until that is wired,
	// auto-approving inside the editor's workspace matches what an editor
	// extension does today.
	a.Kernel.Gate().SetApprover(permission.AutoApprover())

	fmt.Fprintln(os.Stderr, "darkcode: serving the Agent Client Protocol on stdio")
	agent := acp.NewAgent(acpExecutor{runner: a}, os.Stdout)
	if err := agent.Serve(context.Background(), os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "darkcode: acp session ended: %v\n", err)
	}
}
