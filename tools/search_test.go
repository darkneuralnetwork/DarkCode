package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestListFilesDoesNotRunAShell is the regression. ListFiles built a
// `bash -c` string with the model-supplied path interpolated, so a path of
// `. ; touch /tmp/x ; echo ` ran that command. Verified before the rewrite.
func TestListFilesDoesNotRunAShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "INJECTED")

	for _, inject := range []string{
		". ; touch " + marker + " ; echo ",
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		". && touch " + marker,
	} {
		(&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{"path": inject})
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("path %q executed a shell command", inject)
		}
	}
	// The pattern is model-supplied too.
	(&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{
		"path": dir, "pattern": "*' ; touch " + marker + " ; echo '",
	})
	if _, err := os.Stat(marker); err == nil {
		t.Error("the pattern executed a shell command")
	}
}

// TestListFilesHidesAgentState — asked what was in a workspace, the agent
// listed its own memory stores and spilled tool results back to itself.
func TestListFilesHidesAgentState(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "app.go"), "package main")
	mustWrite(t, filepath.Join(dir, ".darkcode", "memory", "episodic.json"), "[]")
	mustWrite(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), "//")

	out := (&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{"path": dir})
	if !out.Success {
		t.Fatalf("ListFiles: %s", out.Error)
	}
	if !strings.Contains(out.Output, "app.go") {
		t.Errorf("the user's own file is missing:\n%s", out.Output)
	}
	for _, leak := range []string{".darkcode", "episodic.json", "node_modules"} {
		if strings.Contains(out.Output, leak) {
			t.Errorf("listing leaked %q:\n%s", leak, out.Output)
		}
	}
}

// TestListFilesIsDeterministic — a listing that reorders between identical
// calls defeats the answer cache and makes any measurement meaningless.
func TestListFilesIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go", "d.go"} {
		mustWrite(t, filepath.Join(dir, n), "package main")
	}
	first := (&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{"path": dir}).Output
	for i := 0; i < 15; i++ {
		if got := (&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{"path": dir}).Output; got != first {
			t.Fatalf("listing changed between identical calls:\n%q\nvs\n%q", got, first)
		}
	}
}

func TestListFilesMatchesPattern(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "keep.go"), "package main")
	mustWrite(t, filepath.Join(dir, "drop.txt"), "text")

	out := (&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{"path": dir, "pattern": "*.go"})
	if !strings.Contains(out.Output, "keep.go") || strings.Contains(out.Output, "drop.txt") {
		t.Errorf("pattern not applied:\n%s", out.Output)
	}
}

func TestListFilesEmptyIsNotAnError(t *testing.T) {
	out := (&SearchTool{}).ListFiles(context.Background(), map[string]interface{}{
		"path": t.TempDir(), "pattern": "*.nothing",
	})
	if !out.Success {
		t.Errorf("no matches reported as failure: %s", out.Error)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
