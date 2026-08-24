package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// CmdVerificationStage.Verify used to run every command in whatever
// directory the process happened to be launched from — cmd.Dir was never
// set, regardless of the workspace passed to the stage's constructor (which
// only affected IsApplicable's language detection). These tests confirm the
// command itself now runs confined to that workspace.

func TestCmdVerificationStageRunsInWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	stage := &CmdVerificationStage{name: "marker-check", cmd: "cat", args: []string{"marker.txt"}, workspace: dir}
	res, err := stage.Verify(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected pass when confined to a workspace containing marker.txt, got issues: %v", res.Issues)
	}
}

func TestCmdVerificationStageEmptyWorkspaceKeepsProcessCwd(t *testing.T) {
	// Documents the fallback: an empty workspace means "wherever the process
	// happens to be" — unchanged legacy behavior, not a regression.
	stage := &CmdVerificationStage{name: "cwd-check", cmd: "pwd"}
	res, err := stage.Verify(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected pwd to succeed with no workspace set, issues: %v", res.Issues)
	}
}

// TestGoStageBuildsItsOwnWorkspaceNotTheProcessCwd is the discriminating
// regression test: a broken temp module should fail verification. Before the
// fix, GoStage.Verify ignored its workspace and ran `go build ./...` in the
// test binary's own directory (this package, which builds cleanly) instead
// of the temp module, so the broken build would have gone undetected and
// this test would see Passed=true.
func TestGoStageBuildsItsOwnWorkspaceNotTheProcessCwd(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module verifyworkspacefixture\n\ngo 1.22\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() { this is not valid go }\n")

	stage := NewGoStage("compiler", "go", []string{"build", "./..."}, dir)
	if !stage.IsApplicable("") {
		t.Fatal("expected IsApplicable true for a directory with go.mod")
	}
	res, err := stage.Verify(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if res.Passed {
		t.Fatal("expected the broken temp module to fail verification; it passed, meaning the build ran somewhere else")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
