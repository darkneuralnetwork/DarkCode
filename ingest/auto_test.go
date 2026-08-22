package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/memory"
)

// countingMem wraps a real memory.System so a test can count how many chunks
// were actually written — the proxy for how many embedding calls a pass costs,
// which is the whole reason this path is incremental.
func newAutoTest(t *testing.T) (*Auto, *memory.System, string) {
	t.Helper()
	root := t.TempDir()
	memDir := t.TempDir()
	sys, err := memory.NewSystem(memDir)
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	t.Cleanup(sys.Shutdown)
	return NewAuto(New(sys, sys.KG()), root, memDir), sys, root
}

func write(t *testing.T, root, name, body string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSecondPassCostsNothing is the point of the manifest. A full pass is one
// embedding call per stored chunk; on a hosted embedder that is the bill. The
// steady state — a restart, a watcher tick on an untouched tree — must cost a
// content hash and no embedding at all.
func TestSecondPassCostsNothing(t *testing.T) {
	auto, _, root := newAutoTest(t)
	write(t, root, "a.go", "package a\n\n"+strings.Repeat("// some real content here\n", 100))
	write(t, root, "b.md", strings.Repeat("documentation paragraph.\n", 100))

	first := auto.Sync(context.Background())
	if first.Chunks == 0 {
		t.Fatal("first pass stored nothing")
	}

	second := auto.Sync(context.Background())
	if second.Chunks != 0 {
		t.Errorf("second pass re-embedded %d chunk(s); unchanged files must cost nothing", second.Chunks)
	}
	if second.Sources != 0 {
		t.Errorf("second pass re-ingested %d source(s), want 0", second.Sources)
	}
}

// TestChangedFileIsReIngested — the other half: a real edit must be picked up.
func TestChangedFileIsReIngested(t *testing.T) {
	auto, _, root := newAutoTest(t)
	write(t, root, "a.go", "package a\n// original\n")
	auto.Sync(context.Background())

	write(t, root, "a.go", "package a\n// rewritten with different content entirely\n")
	after := auto.Sync(context.Background())
	if after.Sources != 1 {
		t.Errorf("changed file produced %d source(s), want 1", after.Sources)
	}
}

// TestShrunkFileLeavesNoStaleChunks. Chunk keys encode their index, so a file
// that goes from five chunks to two leaves #2..#4 unreachable by any future
// write — and retrievable forever as text the file no longer contains.
func TestShrunkFileLeavesNoStaleChunks(t *testing.T) {
	auto, sys, root := newAutoTest(t)
	long := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 400)
	write(t, root, "big.md", long)
	first := auto.Sync(context.Background())
	if first.Chunks < 2 {
		t.Fatalf("fixture produced %d chunk(s); need several to test pruning", first.Chunks)
	}

	write(t, root, "big.md", "tiny now.")
	auto.Sync(context.Background())

	stale := 0
	for _, e := range sys.SemanticAll() {
		if strings.Contains(e.Key, "big.md") && strings.Contains(e.Content, "quick brown fox") {
			stale++
		}
	}
	if stale != 0 {
		t.Errorf("%d chunk(s) of the old, longer version are still indexed", stale)
	}
}

// TestDeletedFileIsForgotten. A deleted file whose chunks stay indexed is a
// source that answers questions about code that no longer exists.
func TestDeletedFileIsForgotten(t *testing.T) {
	auto, sys, root := newAutoTest(t)
	p := write(t, root, "gone.md", strings.Repeat("soon to be deleted content.\n", 50))
	auto.Sync(context.Background())
	if !hasChunkFor(sys, "gone.md") {
		t.Fatal("fixture was never indexed")
	}

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	auto.Sync(context.Background())

	if hasChunkFor(sys, "gone.md") {
		t.Error("a deleted file is still indexed")
	}
}

// TestSkipsDependencyAndBuildDirs — indexing node_modules would dominate the
// cost and teach the retriever nothing about the user's own code.
func TestSkipsDependencyAndBuildDirs(t *testing.T) {
	auto, sys, root := newAutoTest(t)
	write(t, root, "src/main.go", "package main\n"+strings.Repeat("// real\n", 50))
	write(t, root, "node_modules/dep/index.js", strings.Repeat("// vendored\n", 50))
	write(t, root, ".git/config", strings.Repeat("# vcs\n", 50))

	auto.Sync(context.Background())

	for _, forbidden := range []string{"node_modules", ".git"} {
		if hasChunkFor(sys, forbidden) {
			t.Errorf("indexed %s, which should be skipped", forbidden)
		}
	}
	if !hasChunkFor(sys, "main.go") {
		t.Error("the user's own source was not indexed")
	}
}

// TestManifestSurvivesRestart: a new Auto over the same memory dir must not
// re-embed a tree it already indexed. Without this the cost is paid on every
// process start.
func TestManifestSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	memDir := t.TempDir()
	sys, err := memory.NewSystem(memDir)
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	defer sys.Shutdown()

	write(t, root, "a.go", "package a\n"+strings.Repeat("// content\n", 80))

	first := NewAuto(New(sys, sys.KG()), root, memDir).Sync(context.Background())
	if first.Chunks == 0 {
		t.Fatal("first run indexed nothing")
	}

	// A fresh Auto, as a restart would build.
	second := NewAuto(New(sys, sys.KG()), root, memDir).Sync(context.Background())
	if second.Chunks != 0 {
		t.Errorf("after restart, re-embedded %d chunk(s); the manifest should have prevented it", second.Chunks)
	}
}

func hasChunkFor(sys *memory.System, needle string) bool {
	for _, e := range sys.SemanticAll() {
		if strings.Contains(e.Key, needle) {
			return true
		}
	}
	return false
}
