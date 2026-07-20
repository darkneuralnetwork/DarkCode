package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/security"
)

func TestTerminalStrictRefusesWithoutBackend(t *testing.T) {
	// strict mode + no backend must fail closed instead of running unconfined.
	sb := &security.Sandbox{Mode: security.ModeStrict, Backend: security.BackendNone}
	term := NewTerminalTool(sb)

	res := term.Execute(context.Background(), map[string]interface{}{"command": "echo hi"})
	if res.Success {
		t.Fatal("strict sandbox with no backend must refuse to run the command")
	}
	if !strings.Contains(res.Error, "strict") {
		t.Errorf("refusal should explain the strict-mode cause, got %q", res.Error)
	}
}

func TestTerminalRunsWhenNoSandbox(t *testing.T) {
	// No sandbox injected => command runs (unconfined), preserving old behavior.
	term := NewTerminalTool(nil)
	res := term.Execute(context.Background(), map[string]interface{}{"command": "echo darkcode-ok"})
	if !res.Success || !strings.Contains(res.Output, "darkcode-ok") {
		t.Fatalf("plain command should run: success=%v out=%q err=%q", res.Success, res.Output, res.Error)
	}
}
