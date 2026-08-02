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

	// No workspace in context → confined to the working directory.
	//
	// This assertion used to read "should be a no-op, preserves CLI behavior".
	// That behaviour was the bug: core.WorkspaceKey is set only by the HTTP
	// handlers and the ACP entry point, so on the whole CLI surface the guard
	// returned nil and the agent could write anywhere the user could. The test
	// was encoding the hole as the contract, which is why it survived review.
	if err := confineWrite(context.Background(), "/etc/passwd"); err == nil {
		t.Error("a write to /etc with no workspace in context must be refused, not permitted")
	}
	// …but the working directory itself stays writable, so ordinary use is
	// unaffected.
	if err := confineWrite(context.Background(), "./scratch.txt"); err != nil {
		t.Errorf("a write inside the working directory must still be allowed, got %v", err)
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
