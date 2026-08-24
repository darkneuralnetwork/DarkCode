package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestConfineToWorkspaceBlocksSymlinkEscape covers the gap CodeQL found in
// handleFilesRead's old hand-rolled check: filepath.Join + a lexical prefix
// comparison collapses "../" but never looks at symlinks, so a link planted
// inside the workspace pointing outside it passed the confinement check
// under its own name and was then opened under its target's.
func TestConfineToWorkspaceBlocksSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on windows")
	}
	ws := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	if _, err := confineToWorkspace(ws, "escape"); err == nil {
		t.Error("confineToWorkspace followed a symlink out of the workspace instead of blocking it")
	}
}

// TestConfineToWorkspaceAllowsOrdinaryFiles is the companion positive case —
// a fix that blocks everything is as useless as one that blocks nothing.
func TestConfineToWorkspaceAllowsOrdinaryFiles(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "b.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"a.txt", "sub/b.txt", "sub"} {
		abs, err := confineToWorkspace(ws, rel)
		if err != nil {
			t.Errorf("confineToWorkspace(%q) rejected an ordinary in-workspace path: %v", rel, err)
			continue
		}
		want := filepath.Join(ws, rel)
		if wantReal, rerr := filepath.EvalSymlinks(want); rerr == nil {
			want = wantReal
		}
		if abs != want {
			t.Errorf("confineToWorkspace(%q) = %q, want %q", rel, abs, want)
		}
	}
}

// TestConfineToWorkspaceBlocksDotDotTraversal is the ordinary case the old
// check already handled — kept as a regression guard for the rewrite.
func TestConfineToWorkspaceBlocksDotDotTraversal(t *testing.T) {
	ws := t.TempDir()
	for _, rel := range []string{"../../etc/passwd", "..", "sub/../../escape"} {
		if _, err := confineToWorkspace(ws, rel); err == nil {
			t.Errorf("confineToWorkspace(%q) was not blocked", rel)
		}
	}
}
