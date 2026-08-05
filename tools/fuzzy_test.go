package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindSnippetExactMatchWins(t *testing.T) {
	content := "line one\nline two\nline three\n"
	m := findSnippet(content, "line two")
	if m.err != "" || m.fuzzy {
		t.Fatalf("expected a clean exact match, got %+v", m)
	}
	if content[m.start:m.end] != "line two" {
		t.Errorf("matched %q", content[m.start:m.end])
	}
}

// The common real failure: the model reproduces the snippet with different
// indentation.
func TestFindSnippetToleratesIndentation(t *testing.T) {
	content := "func f() {\n\t\tif x {\n\t\t\treturn 1\n\t\t}\n}\n"
	m := findSnippet(content, "if x {\nreturn 1\n}")
	if m.err != "" {
		t.Fatalf("whitespace-different snippet was rejected: %s", m.err)
	}
	if !m.fuzzy {
		t.Error("match should be reported as fuzzy")
	}
	got := content[m.start:m.end]
	if !strings.Contains(got, "return 1") || !strings.Contains(got, "if x {") {
		t.Errorf("matched the wrong region: %q", got)
	}
}

func TestFindSnippetToleratesTrailingWhitespace(t *testing.T) {
	content := "alpha   \nbeta\t\ngamma\n"
	if m := findSnippet(content, "alpha\nbeta"); m.err != "" {
		t.Errorf("trailing whitespace should not block a match: %s", m.err)
	}
}

// Patching the wrong region is worse than not patching, so an ambiguous
// relaxed match must fail loudly.
func TestFindSnippetRefusesAmbiguousFuzzyMatch(t *testing.T) {
	// Both blocks are indented, so neither is an exact substring of the
	// snippet — but both match once whitespace is ignored.
	content := "  foo()\n  bar()\n\n\tfoo()\n\tbar()\n"
	m := findSnippet(content, "foo()\nbar()")
	if m.err == "" {
		t.Fatal("two whitespace-insensitive matches should be an error")
	}
	if !strings.Contains(m.err, "2 places") {
		t.Errorf("error should say how many places matched: %q", m.err)
	}
}

func TestFindSnippetReportsGenuineMiss(t *testing.T) {
	m := findSnippet("hello world\n", "not present at all")
	if m.err == "" {
		t.Fatal("expected an error for a missing snippet")
	}
}

// End-to-end: the patch tool applies a whitespace-different snippet and leaves
// the rest of the file intact.
func TestPatchAppliesFuzzyMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n    println(\"old\")\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// A workspace is required now: confineWrite fails closed without one, so a
	// bare context is no longer a shape any real caller produces.
	res := (&FileTool{}).PatchFile(withWorkspace(dir), map[string]interface{}{
		"path":       path,
		"old_string": "println(\"old\")",
		"new_string": "    println(\"new\")",
	})
	if !res.Success {
		t.Fatalf("patch failed: %s", res.Error)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "old") {
		t.Errorf("old content survived:\n%s", got)
	}
	if !strings.Contains(got, "println(\"new\")") {
		t.Errorf("new content missing:\n%s", got)
	}
	if !strings.HasPrefix(got, "package main\n") || !strings.HasSuffix(got, "}\n") {
		t.Errorf("surrounding file was damaged:\n%s", got)
	}
}

func TestPatchStillReportsMissingSnippet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := (&FileTool{}).PatchFile(context.Background(), map[string]interface{}{
		"path": path, "old_string": "goodbye", "new_string": "hi",
	})
	if res.Success {
		t.Error("patching a snippet that does not exist should fail")
	}
}
