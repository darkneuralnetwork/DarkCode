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

	// No workspace in context → always allowed (preserves CLI behavior).
	if err := confineWrite(context.Background(), "/etc/passwd"); err != nil {
		t.Errorf("no-workspace confineWrite should be a no-op, got %v", err)
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
