package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/core"
)

func obsSystem(t *testing.T) *System {
	t.Helper()
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestUncommittedEditIsStale is the regression, and the case that matters most.
//
// StaleFiles compared the recorded commit to HEAD. Editing a file without
// committing does not move HEAD, so the graph kept asserting the old contents
// — including right after the agent edited the file itself, which is the most
// likely moment for a belief to be wrong.
func TestUncommittedEditIsStale(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()
	path := filepath.Join(ws, "app.go")

	if err := os.WriteFile(path, []byte("package main // v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.ObserveFile(ws, path, "package main // v1")

	if stale := s.KG().(*KnowledgeGraph).StaleFiles(ws); len(stale) != 0 {
		t.Fatalf("an unchanged file is reported stale: %+v", stale)
	}

	// Edit it, commit nothing — exactly what the agent does mid-task.
	if err := os.WriteFile(path, []byte("package main // v2 EDITED"), 0o644); err != nil {
		t.Fatal(err)
	}

	stale := s.KG().(*KnowledgeGraph).StaleFiles(ws)
	if len(stale) != 1 {
		t.Fatalf("an edited file is not stale: got %d entries — the graph is still "+
			"asserting contents that no longer exist", len(stale))
	}
	if stale[0].Provenance == "" {
		t.Error("staleness reported without saying what changed")
	}
}

// TestUnreadFilesAreNotStale — never-read is not stale, it is unknown.
// Reporting it would bury the files that genuinely changed under every file in
// the repository, which is what made the commit-based answer unreadable.
func TestUnreadFilesAreNotStale(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "never_read.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stale := s.KG().(*KnowledgeGraph).StaleFiles(ws); len(stale) != 0 {
		t.Errorf("a file the agent never read is reported stale: %+v", stale)
	}
}

// TestDeletedFileIsStale — a belief about a file that is gone is the most
// wrong a belief can be.
func TestDeletedFileIsStale(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()
	path := filepath.Join(ws, "gone.go")
	if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.ObserveFile(ws, path, "package main")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if stale := s.KG().(*KnowledgeGraph).StaleFiles(ws); len(stale) != 1 {
		t.Errorf("a deleted file is not reported stale: got %d", len(stale))
	}
}

// TestReObservingRefreshes — reading a file again is what makes the belief
// current, so knowledge tracks attention.
func TestReObservingRefreshes(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()
	path := filepath.Join(ws, "a.go")

	os.WriteFile(path, []byte("v1"), 0o644)
	s.ObserveFile(ws, path, "v1")
	os.WriteFile(path, []byte("v2"), 0o644)
	if len(s.KG().(*KnowledgeGraph).StaleFiles(ws)) != 1 {
		t.Fatal("setup: expected the edit to be stale")
	}

	s.ObserveFile(ws, path, "v2") // the agent reads it again
	if stale := s.KG().(*KnowledgeGraph).StaleFiles(ws); len(stale) != 0 {
		t.Errorf("re-reading did not refresh the belief: %+v", stale)
	}
}

// TestObservationPreservesIndexerProperties — observing content must not erase
// what the indexer recorded about the file.
func TestObservationPreservesIndexerProperties(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()
	kg := s.KG()

	_ = kg.AddNode(&core.KGNode{
		ID: "file:a.go", Label: "a.go", Type: core.KGNodeFile,
		Properties: map[string]string{"language": "go", "symbols": "12"},
		Confidence: 1.0,
	})
	s.ObserveFile(ws, filepath.Join(ws, "a.go"), "package main")

	n, ok := kg.GetNode("file:a.go")
	if !ok {
		t.Fatal("node disappeared")
	}
	if n.Properties["language"] != "go" || n.Properties["symbols"] != "12" {
		t.Errorf("observing content erased the indexer's properties: %v", n.Properties)
	}
	if n.Properties[fileHashProperty] == "" {
		t.Error("content hash was not recorded")
	}
}

// TestFilesOutsideTheWorkspaceAreNotRecorded — a file index shared between
// projects describes neither.
func TestFilesOutsideTheWorkspaceAreNotRecorded(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.go")

	s.ObserveFile(ws, outside, "package other")

	for _, n := range s.KG().FindByType(core.KGNodeFile) {
		if n.Properties[fileHashProperty] != "" {
			t.Errorf("recorded a file outside the workspace: %s", n.Label)
		}
	}
}

func TestFileChangedReportsUnknownSeparately(t *testing.T) {
	s := obsSystem(t)
	ws := t.TempDir()

	if _, known := s.FileChanged(ws, filepath.Join(ws, "never.go"), "x"); known {
		t.Error("an unread file reported as known — unknown is not the same as unchanged")
	}
	s.ObserveFile(ws, filepath.Join(ws, "seen.go"), "v1")
	changed, known := s.FileChanged(ws, filepath.Join(ws, "seen.go"), "v2")
	if !known || !changed {
		t.Errorf("changed=%v known=%v, want both true", changed, known)
	}
}
