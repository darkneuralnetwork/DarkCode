package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/memory"
)

func TestChunk(t *testing.T) {
	// Short text → single chunk.
	if got := chunk("hello world", 1500, 200); len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("short text should be one chunk, got %v", got)
	}
	// Long text → multiple chunks with overlap, covering all content.
	long := strings.Repeat("paragraph line one.\nparagraph line two.\n\n", 200) // ~8KB
	chunks := chunk(long, 1500, 200)
	if len(chunks) < 4 {
		t.Fatalf("expected several chunks for ~8KB, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) == 0 {
			t.Fatalf("chunk %d is empty", i)
		}
		if len(c) > 1500+200 {
			t.Fatalf("chunk %d too large: %d", i, len(c))
		}
	}
}

func TestExtensionClassification(t *testing.T) {
	if !isCodeExt("main.go") || !isCodeExt("app.py") {
		t.Error("code extensions misclassified")
	}
	if isCodeExt("README.md") {
		t.Error("README.md is not code")
	}
	if !ingestibleExt("notes.md") || !ingestibleExt("main.go") {
		t.Error("md/go should be ingestible")
	}
	if ingestibleExt("photo.png") || ingestibleExt("archive.zip") {
		t.Error("binaries should not be ingestible")
	}
	if !skipDir(".git") || !skipDir("node_modules") || skipDir("src") {
		t.Error("skipDir logic wrong")
	}
	if !isBinary([]byte{'a', 0, 'b'}) || isBinary([]byte("plain text")) {
		t.Error("isBinary logic wrong")
	}
}

func newMem(t *testing.T) *memory.System {
	t.Helper()
	m, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("memory.NewSystem: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func TestIngestText(t *testing.T) {
	mem := newMem(t)
	in := New(mem, nil)

	st, err := in.IngestText(context.Background(), "my-notes", "note", "Goroutines are lightweight threads managed by the Go runtime.")
	if err != nil {
		t.Fatal(err)
	}
	if st.Chunks != 1 || st.Sources != 1 {
		t.Fatalf("expected 1 chunk / 1 source, got %+v", st)
	}
	if len(mem.SemanticAll()) != 1 {
		t.Fatalf("expected 1 semantic entry, got %d", len(mem.SemanticAll()))
	}
}

func TestIngestDir(t *testing.T) {
	dir := t.TempDir()
	// A code file, a doc file, a binary (skipped), and a skipped subdir.
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Project\n\nThis explains the design.\n"), 0644)
	os.WriteFile(filepath.Join(dir, "logo.png"), []byte{0, 1, 2, 0}, 0644)
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0755)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "dep.js"), []byte("module.exports = 1;"), 0644)

	mem := newMem(t)
	in := New(mem, nil)
	st, err := in.IngestDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sources != 2 { // main.go + README.md; png and node_modules excluded
		t.Fatalf("expected 2 sources ingested, got %d (%+v)", st.Sources, st)
	}

	// The code file must be categorized as code.
	var sawCode, sawDoc bool
	for _, e := range mem.SemanticAll() {
		if e.Category == "code" {
			sawCode = true
		}
		if e.Category == "doc" {
			sawDoc = true
		}
	}
	if !sawCode || !sawDoc {
		t.Fatalf("expected both code and doc categories, code=%v doc=%v", sawCode, sawDoc)
	}
}

func TestIngestAutoDetectRejectsUnsafeURL(t *testing.T) {
	in := New(newMem(t), nil)
	if _, err := in.Ingest(context.Background(), "http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("ingesting a cloud-metadata URL should be refused by the SSRF guard")
	}
}
