package main

import (
	"context"
	"fmt"
	"os"

	"github.com/darkcode/acp"
	"github.com/darkcode/permission"
	"github.com/darkcode/uiport"
)

// acpExecutor adapts the orchestration kernel to the ACP agent's Executor
// interface, scoping each turn to the session's working directory.
type acpExecutor struct{ runner *AppRunner }

func (e acpExecutor) Execute(ctx context.Context, cwd, prompt string) (string, error) {
	// The editor names the session's working directory. When it doesn't, fall
	// back to ours rather than running unconfined: uiport refuses a request
	// with no workspace, so the failure reaches the editor instead of becoming
	// an agent that can write anywhere.
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("acp: no session cwd and the working directory is unreadable: %w", err)
		}
		cwd = wd
	}
	return e.runner.Port.Execute(ctx, uiport.Request{
		Query:     prompt,
		Surface:   uiport.SurfaceACP,
		Workspace: cwd,
	})
}

// RunACP serves the Agent Client Protocol on stdio until the editor
// disconnects.
//
// stdout is the protocol channel, so nothing else may write to it — a stray
// log line is a malformed message that desynchronises the stream. Logs go to
// stderr, which editors surface separately.
func (a *AppRunner) RunACP() {
	defer a.gracefulShutdown()

	fmt.Fprintln(os.Stderr, "darkcode: serving the Agent Client Protocol on stdio")
	agent := acp.NewAgent(acpExecutor{runner: a}, os.Stdout)

	// Dangerous actions go to the editor's own approval UI. Anything that is
	// not an explicit approval — an editor that does not implement the method,
	// a timeout, a cancellation — denies, so running under an editor is never
	// the loosest way to run the agent.
	a.Kernel.Gate().SetApprover(acpApprover(agent))

	if err := agent.Serve(context.Background(), os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "darkcode: acp session ended: %v\n", err)
	}
}

// acpApprover routes gate approvals through ACP's session/request_permission.
func acpApprover(agent *acp.Agent) permission.Approver {
	return func(req permission.ApprovalRequest) permission.Verdict {
		option, err := agent.RequestPermission(context.Background(), acp.PermissionRequest{
			Title:   approvalTitle(req),
			Kind:    acpToolKind(req.Tool),
			Content: req.Preview,
		})
		if err != nil {
			// Includes ErrNoSession, a timeout, and an editor that answered
			// "cancelled". Denying is the only safe reading of all of them.
			return permission.DenyV("not approved in the editor: " + err.Error())
		}
		switch option {
		case acp.OptAllowOnce:
			return permission.AllowV(permission.DecisionAllowOnce)
		case acp.OptAllowAlways:
			return permission.AllowV(permission.DecisionAllowSession)
		default:
			return permission.DenyV("rejected in the editor")
		}
	}
}

// approvalTitle is the one line the user reads before deciding, so it names
// the tool and what the gate thought was risky about the call.
func approvalTitle(req permission.ApprovalRequest) string {
	if req.Summary != "" {
		return req.Summary
	}
	return "Run " + req.Tool
}

// acpToolKind maps a tool onto the kinds an editor knows how to render. The
// icon and colour an editor picks come from this, so a wrong answer is
// misleading rather than merely untidy.
func acpToolKind(tool string) string {
	switch tool {
	case "write_file", "edit_file", "patch", "apply_patch":
		return "edit"
	case "terminal", "bash", "debug":
		return "execute"
	case "read_file", "list_files", "search":
		return "read"
	case "web_fetch", "web_search", "research", "github":
		return "fetch"
	default:
		return "other"
	}
}
