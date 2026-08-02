package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/darkcode/core"
)

// TestWriteOutsideWorkspaceIsRefusedWithoutProject is W1 contract test 1.
//
// confineWrite currently opens with `if ws == "" { return nil }`, and
// core.WorkspaceKey is injected only by the server handlers and app_acp.go —
// never on the CLI path. So on the entire terminal surface the guard is a
// no-op and the agent can write /etc, ~/.ssh/authorized_keys, anywhere the
// user can. The sandbox does not cover this: it confines shell commands, while
// write_file/patch/replace_file_content are native Go and rely on this check
// alone.
//
// Fails until the admission port makes a workspace mandatory (T-06/T-09).
func TestWriteOutsideWorkspaceIsRefusedWithoutProject(t *testing.T) {
	err := confineWrite(context.Background(), "/etc/darkcode-should-not-write")
	if err == nil {
		t.Fatal("a write to /etc was permitted with no workspace in context — " +
			"path confinement is inert on every code path that does not set core.WorkspaceKey")
	}
}

// TestWriteInsideWorkspaceStillAllowed is the other half: the fix must not
// make legitimate writes fail. Without this, "refuse everything" would pass
// the test above.
func TestWriteInsideWorkspaceStillAllowed(t *testing.T) {
	ws := t.TempDir()
	ctx := context.WithValue(context.Background(), core.WorkspaceKey, ws)

	if err := confineWrite(ctx, filepath.Join(ws, "sub", "file.go")); err != nil {
		t.Fatalf("a write inside the active workspace must be allowed, got %v", err)
	}
	if err := confineWrite(ctx, filepath.Join(ws, "..", "escape.txt")); err == nil {
		t.Error("a traversal out of the workspace must still be refused")
	}
}
