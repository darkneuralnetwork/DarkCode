package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/core"
)

func withWorkspace(ws string) context.Context {
	return context.WithValue(context.Background(), core.WorkspaceKey, ws)
}

func TestConfineWrite(t *testing.T) {
	ws := t.TempDir()

	// INVERTED. This assertion used to require the opposite — that a request
	// with no workspace permits any write — under the comment "preserves CLI
	// behavior". It encoded the bug: no CLI surface ever set a workspace, so
	// what it preserved was the absence of confinement everywhere but the GUI.
	// Measured on a live binary before this change, POST /api/tools/execute
	// wrote a file outside the workspace and reported success.
	//
	// The target below is deliberately NOT /etc/passwd. The original check used
	// exactly that, and an unprivileged process cannot write it whether or not
	// the guard exists — so that assertion passed for the wrong reason and
	// could never have caught this. Only a writable path tests the guard.
	writable := filepath.Join(t.TempDir(), "reachable.txt")
	if err := confineWrite(context.Background(), writable); err == nil {
		t.Error("confineWrite permitted a write with no workspace on the request — " +
			"a guard whose default is to permit is not a guard")
	}

	ctx := withWorkspace(ws)
	allowed := []string{
		filepath.Join(ws, "main.go"),
		filepath.Join(ws, "pkg", "deep", "file.txt"),
		ws, // the root itself
	}
	for _, p := range allowed {
		if err := confineWrite(ctx, p); err != nil {
			t.Errorf("confineWrite(%q) inside workspace should pass, got %v", p, err)
		}
	}

	blocked := []string{
		"/etc/passwd",
		filepath.Join(ws, "..", "escape.txt"),
		filepath.Join(ws, "..", filepath.Base(ws)+"-sibling", "x"),
	}
	for _, p := range blocked {
		if err := confineWrite(ctx, p); err == nil {
			t.Errorf("confineWrite(%q) outside workspace should be blocked", p)
		}
	}
}

// TestConfineWriteSymlinkEscape verifies a symlink inside the workspace can't
// redirect a write to a target outside it.
func TestConfineWriteSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	ctx := withWorkspace(ws)

	// A symlink "ws/escape" -> outside dir. Writing through it must be blocked.
	link := filepath.Join(ws, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := confineWrite(ctx, filepath.Join(link, "payload.txt")); err == nil {
		t.Errorf("write through symlink escaping the workspace should be blocked")
	}

	// A symlink pointing within the workspace stays allowed.
	inside := filepath.Join(ws, "sub")
	if err := os.Mkdir(inside, 0755); err != nil {
		t.Fatal(err)
	}
	innerLink := filepath.Join(ws, "innerlink")
	if err := os.Symlink(inside, innerLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := confineWrite(ctx, filepath.Join(innerLink, "ok.txt")); err != nil {
		t.Errorf("write through in-workspace symlink should pass, got %v", err)
	}
}
