package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestManager returns a manager over a fresh workspace and store.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	ws := t.TempDir()
	m, err := New(t.TempDir(), ws)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, ws
}

func write(t *testing.T, ws, rel, content string) {
	t.Helper()
	path := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ws, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// A rollback must restore modified files, delete files created afterwards, and
// bring back files that were deleted.
func TestRollbackRestoresWorkspace(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "keep.txt", "original")
	write(t, ws, "gone.txt", "delete me later")

	if _, err := m.Snapshot("test", "before edits"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	write(t, ws, "keep.txt", "modified")
	write(t, ws, "new/added.txt", "created after the snapshot")
	if err := os.Remove(filepath.Join(ws, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := m.Rollback(1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := read(t, ws, "keep.txt"); got != "original" {
		t.Errorf("keep.txt = %q, want %q", got, "original")
	}
	if got := read(t, ws, "gone.txt"); got != "delete me later" {
		t.Errorf("gone.txt = %q, want it restored", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "new/added.txt")); !os.IsNotExist(err) {
		t.Error("added.txt should have been removed by the rollback")
	}
}

// The rollback itself is snapshotted first, so the undo can be undone.
func TestRollbackIsUndoable(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "f.txt", "v1")
	if _, err := m.Snapshot("test", "v1"); err != nil {
		t.Fatal(err)
	}
	write(t, ws, "f.txt", "v2")

	if _, _, err := m.Rollback(1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := read(t, ws, "f.txt"); got != "v1" {
		t.Fatalf("after rollback f.txt = %q, want v1", got)
	}

	// Checkpoint 2 is the automatic pre-rollback snapshot holding "v2".
	if _, _, err := m.Rollback(2); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if got := read(t, ws, "f.txt"); got != "v2" {
		t.Errorf("after undoing the undo f.txt = %q, want v2", got)
	}
}

func TestRollbackFileRestoresOnlyThatFile(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "a.txt", "a1")
	write(t, ws, "b.txt", "b1")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}
	write(t, ws, "a.txt", "a2")
	write(t, ws, "b.txt", "b2")

	if err := m.RollbackFile(1, "a.txt"); err != nil {
		t.Fatalf("RollbackFile: %v", err)
	}
	if got := read(t, ws, "a.txt"); got != "a1" {
		t.Errorf("a.txt = %q, want a1", got)
	}
	if got := read(t, ws, "b.txt"); got != "b2" {
		t.Errorf("b.txt = %q, want b2 (untouched)", got)
	}
}

func TestDiffReportsEachChangeKind(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "mod.txt", "before")
	write(t, ws, "del.txt", "x")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}
	write(t, ws, "mod.txt", "after")
	write(t, ws, "new.txt", "y")
	if err := os.Remove(filepath.Join(ws, "del.txt")); err != nil {
		t.Fatal(err)
	}

	changes, _, err := m.Diff(1)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Status
	}
	for path, want := range map[string]string{
		"mod.txt": "modified", "new.txt": "created", "del.txt": "deleted",
	} {
		if got[path] != want {
			t.Errorf("%s: got %q, want %q", path, got[path], want)
		}
	}
}

// Turn is what lets a rollback rewind the conversation with the filesystem.
func TestSnapshotRecordsConversationTurn(t *testing.T) {
	m, ws := newTestManager(t)
	turn := 0
	m.SetTurnFunc(func() int { return turn })

	write(t, ws, "f.txt", "v1")
	turn = 7
	e, err := m.Snapshot("test", "at turn 7")
	if err != nil {
		t.Fatal(err)
	}
	if e.Turn != 7 {
		t.Errorf("Turn = %d, want 7", e.Turn)
	}
}

// Blobs are content-addressed, so re-snapshotting unchanged content must not
// write a second copy — this is what keeps always-on checkpointing affordable.
func TestBlobsAreDeduplicated(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "a.txt", "same bytes")
	write(t, ws, "b.txt", "same bytes")
	if _, err := m.Snapshot("test", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot("test", "two"); err != nil {
		t.Fatal(err)
	}

	blobs := 0
	_ = filepath.WalkDir(filepath.Join(m.root, "store"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Errorf("stored %d blobs, want 1 (identical content across 2 files and 2 snapshots)", blobs)
	}
}

// Excluded directories must be neither captured nor deleted by a rollback.
func TestSkippedDirsSurviveRollback(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "src.txt", "code")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}
	write(t, ws, "node_modules/dep/index.js", "vendored")
	write(t, ws, ".git/HEAD", "ref: refs/heads/main")

	if _, _, err := m.Rollback(1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for _, rel := range []string{"node_modules/dep/index.js", ".git/HEAD"} {
		if _, err := os.Stat(filepath.Join(ws, rel)); err != nil {
			t.Errorf("%s should have survived the rollback: %v", rel, err)
		}
	}
}

// Checkpoints must survive a restart, otherwise undo dies with the process.
func TestLogPersistsAcrossManagers(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	m, err := New(root, ws)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws, "f.txt", "v1")
	if _, err := m.Snapshot("test", "persisted"); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(root, ws)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.List()
	if len(entries) != 1 || entries[0].Label != "persisted" {
		t.Fatalf("reopened log = %+v, want the one persisted entry", entries)
	}

	write(t, ws, "f.txt", "v2")
	if _, _, err := reopened.Rollback(1); err != nil {
		t.Fatalf("Rollback after reopen: %v", err)
	}
	if got := read(t, ws, "f.txt"); got != "v1" {
		t.Errorf("f.txt = %q, want v1", got)
	}
}

// Dotted directories that are not build/VCS noise (notably .github) must be
// checkpointed like any other source.
func TestDottedProjectDirsAreCheckpointed(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, ".github/workflows/ci.yml", "on: push")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}
	write(t, ws, ".github/workflows/ci.yml", "on: pull_request")

	if _, _, err := m.Rollback(1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := read(t, ws, ".github/workflows/ci.yml"); got != "on: push" {
		t.Errorf("ci.yml = %q, want the checkpointed content", got)
	}
}

// A traversal names a path no checkpoint recorded, which is exactly the branch
// that treats "unrecorded" as "created afterwards, so delete it". Rolling one
// back must be refused rather than reaching outside the workspace.
func TestRollbackFileRefusesPathsOutsideWorkspace(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "a.txt", "a1")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(ws, outside)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.RollbackFile(1, rel); err == nil {
		t.Errorf("RollbackFile(%q) succeeded, want a containment error", rel)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside the workspace was deleted: %v", err)
	}
}

// The lexical check alone is not enough: a symlink inside the workspace can
// point out of it while the joined path still looks contained.
func TestRollbackFileRefusesSymlinkEscape(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "a.txt", "a1")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	victim := filepath.Join(outsideDir, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(ws, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if err := m.RollbackFile(1, "link/victim.txt"); err == nil {
		t.Error("RollbackFile through a symlink succeeded, want a containment error")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("file outside the workspace was deleted through a symlink: %v", err)
	}
}

// Containment must not break the ordinary case it guards: restoring a file the
// checkpoint recorded as deleted writes a path that does not exist yet, so
// resolution has to tolerate a missing leaf.
func TestRollbackFileRestoresDeletedFile(t *testing.T) {
	m, ws := newTestManager(t)
	write(t, ws, "nested/gone.txt", "original")
	if _, err := m.Snapshot("test", "base"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ws, "nested")); err != nil {
		t.Fatal(err)
	}

	if err := m.RollbackFile(1, "nested/gone.txt"); err != nil {
		t.Fatalf("RollbackFile: %v", err)
	}
	if got := read(t, ws, "nested/gone.txt"); got != "original" {
		t.Errorf("gone.txt = %q, want it restored", got)
	}
}
