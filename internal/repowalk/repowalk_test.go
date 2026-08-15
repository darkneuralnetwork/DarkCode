package repowalk

import "testing"

// TestAgentStateIsNeverWalked is the regression. Asked what was in a
// workspace, the agent listed its own memory stores and spilled tool results
// back to itself, because the file tools skipped nothing.
func TestAgentStateIsNeverWalked(t *testing.T) {
	if !SkipDir(".darkcode") {
		t.Error("the agent's own state directory is walkable")
	}
	for _, p := range []string{
		".darkcode/memory/episodic.json",
		"proj/.darkcode/memory/spill/abc.txt",
		"./.darkcode/checkpoints",
	} {
		if !SkipPath(p) {
			t.Errorf("SkipPath(%q) = false; the agent would read its own bookkeeping", p)
		}
	}
}

func TestSkipsTheUsualNoise(t *testing.T) {
	for _, n := range []string{".git", "node_modules", "vendor", "__pycache__", "target", ".venv", ".idea"} {
		if !SkipDir(n) {
			t.Errorf("SkipDir(%q) = false", n)
		}
	}
}

func TestKeepsRealSource(t *testing.T) {
	for _, n := range []string{"src", "cmd", "internal", "pkg", "lib", "app", "tests", "docs"} {
		if SkipDir(n) {
			t.Errorf("SkipDir(%q) = true; that is somebody's code", n)
		}
	}
	for _, p := range []string{"cmd/root.go", "internal/pkg/thing.go", "src/main.rs"} {
		if SkipPath(p) {
			t.Errorf("SkipPath(%q) = true", p)
		}
	}
}

// TestWalkCanStartAtDot — skipping "." would make a walk rooted at the
// workspace return nothing at all.
func TestWalkCanStartAtDot(t *testing.T) {
	for _, n := range []string{".", "..", ""} {
		if SkipDir(n) {
			t.Errorf("SkipDir(%q) = true; a walk rooted here returns nothing", n)
		}
	}
	if SkipPath("./cmd/root.go") {
		t.Error(`SkipPath("./cmd/root.go") = true`)
	}
}

func TestHiddenDirsAreSkippedAsAClass(t *testing.T) {
	for _, n := range []string{".hidden", ".config", ".anything-new"} {
		if !SkipDir(n) {
			t.Errorf("SkipDir(%q) = false", n)
		}
	}
}
