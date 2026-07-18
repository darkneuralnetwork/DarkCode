package tools

import (
	"context"
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
