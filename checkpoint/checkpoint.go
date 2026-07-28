// Package checkpoint gives the agent an undo button.
//
// Before any mutating tool runs, the workspace is snapshotted into a
// content-addressed blob store shared by every project: a file's bytes are
// stored once under their SHA-256 and referenced by hash from each snapshot,
// so taking a hundred checkpoints of a repository costs roughly the size of
// what actually changed. Unchanged files are detected by (size, mtime) and
// never re-read.
//
// A rollback restores the recorded contents, deletes files created since, and
// reports the conversation length at snapshot time so the caller can rewind
// the transcript along with the filesystem — otherwise the agent keeps acting
// on beliefs about files that no longer exist.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limits on what a snapshot will capture. Files above maxFileSize are recorded
// as present-but-uncaptured (empty hash) so a rollback leaves them alone
// instead of deleting a large artifact it never stored.
const (
	maxFileSize    = 2 << 20 // 2 MiB
	maxFiles       = 20000
	maxCheckpoints = 100
)

// skipDirs are never walked: version control, dependency trees, build output
// and our own state. Restoring them is never what the user means by "undo".
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".darkcode": true,
	"node_modules": true, "vendor": true, "target": true, "dist": true,
	"build": true, "__pycache__": true, ".venv": true, "venv": true,
	".next": true, ".cache": true, ".idea": true, ".gradle": true,
}

// Entry is one checkpoint: the workspace contents plus the conversation
// position, so both can be rewound together.
type Entry struct {
	ID    int               `json:"id"`
	Time  time.Time         `json:"time"`
	Tool  string            `json:"tool"`
	Label string            `json:"label"`
	Turn  int               `json:"turn"`  // message count when taken
	Files map[string]string `json:"files"` // workspace-relative path → blob hash ("" = too large to capture)
}

// Manager records and restores checkpoints for one workspace.
type Manager struct {
	mu        sync.Mutex
	root      string // blob store + logs live here
	workspace string
	entries   []Entry
	cache     map[string]cached // path → last known (size, mtime, hash)
	turn      func() int
}

type cached struct {
	size  int64
	mtime int64
	hash  string
}

// New opens (or creates) the checkpoint store at root for the given workspace.
func New(root, workspace string) (*Manager, error) {
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	m := &Manager{root: root, workspace: ws, cache: map[string]cached{}}
	if err := os.MkdirAll(m.logDir(), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(m.logPath())
	if err == nil {
		_ = json.Unmarshal(data, &m.entries)
	}
	return m, nil
}

// SetTurnFunc installs the callback that reports the current conversation
// length. Without it checkpoints still work; they just can't rewind the
// transcript.
func (m *Manager) SetTurnFunc(fn func() int) {
	m.mu.Lock()
	m.turn = fn
	m.mu.Unlock()
}

func (m *Manager) logDir() string {
	sum := sha256.Sum256([]byte(m.workspace))
	return filepath.Join(m.root, "projects", hex.EncodeToString(sum[:])[:16])
}

func (m *Manager) logPath() string { return filepath.Join(m.logDir(), "log.json") }

func (m *Manager) blobPath(hash string) string {
	return filepath.Join(m.root, "store", hash[:2], hash[2:])
}

// putBlob stores data under its content hash, skipping the write when the blob
// already exists — this is what makes repeated snapshots cheap.
func (m *Manager) putBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := m.blobPath(hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return hash, os.WriteFile(path, data, 0o644)
}

// walk lists the workspace-relative paths a snapshot considers, applying the
// same filter used at restore time so "present now but absent from the
// snapshot" reliably means "created after it".
func (m *Manager) walk() ([]string, error) {
	var paths []string
	err := filepath.WalkDir(m.workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			// Only the known-noisy directories are skipped, not every dotted
			// one: an agent editing .github/workflows must still be undoable.
			if path != m.workspace && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(paths) >= maxFiles {
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(m.workspace, path)
		if rerr == nil {
			paths = append(paths, rel)
		}
		return nil
	})
	return paths, err
}

// Snapshot captures the workspace and appends a checkpoint.
func (m *Manager) Snapshot(tool, label string) (Entry, error) {
	paths, err := m.walk()
	if err != nil {
		return Entry{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	files := make(map[string]string, len(paths))
	for _, rel := range paths {
		abs := filepath.Join(m.workspace, rel)
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if info.Size() > maxFileSize {
			files[rel] = ""
			continue
		}
		// Unchanged since we last hashed it — reuse without reading.
		if c, ok := m.cache[rel]; ok && c.size == info.Size() && c.mtime == info.ModTime().UnixNano() {
			files[rel] = c.hash
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		hash, err := m.putBlob(data)
		if err != nil {
			return Entry{}, err
		}
		files[rel] = hash
		m.cache[rel] = cached{size: info.Size(), mtime: info.ModTime().UnixNano(), hash: hash}
	}

	turn := 0
	if m.turn != nil {
		turn = m.turn()
	}
	id := 1
	if n := len(m.entries); n > 0 {
		id = m.entries[n-1].ID + 1
	}
	e := Entry{ID: id, Time: time.Now(), Tool: tool, Label: label, Turn: turn, Files: files}
	m.entries = append(m.entries, e)
	if len(m.entries) > maxCheckpoints {
		m.entries = m.entries[len(m.entries)-maxCheckpoints:]
	}
	return e, m.saveLocked()
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.logPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.logPath())
}

// List returns the recorded checkpoints, oldest first.
func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out
}

// find resolves a checkpoint id. A non-positive n counts back from the newest,
// so 0 or -1 means "the most recent".
func (m *Manager) find(n int) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return Entry{}, false
	}
	if n <= 0 {
		return m.entries[len(m.entries)-1], true
	}
	for _, e := range m.entries {
		if e.ID == n {
			return e, true
		}
	}
	return Entry{}, false
}

// Change describes one difference between a checkpoint and the working tree.
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "modified" | "created" | "deleted"
}

// Diff reports how the working tree differs from checkpoint n.
func (m *Manager) Diff(n int) ([]Change, Entry, error) {
	e, ok := m.find(n)
	if !ok {
		return nil, Entry{}, fmt.Errorf("no checkpoint %d", n)
	}
	paths, err := m.walk()
	if err != nil {
		return nil, e, err
	}
	var changes []Change
	seen := map[string]bool{}
	for _, rel := range paths {
		seen[rel] = true
		want, recorded := e.Files[rel]
		if !recorded {
			changes = append(changes, Change{rel, "created"})
			continue
		}
		if want == "" {
			continue // uncaptured (oversize) — nothing to compare
		}
		data, err := os.ReadFile(filepath.Join(m.workspace, rel))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			changes = append(changes, Change{rel, "modified"})
		}
	}
	for rel, hash := range e.Files {
		if !seen[rel] && hash != "" {
			changes = append(changes, Change{rel, "deleted"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, e, nil
}

// contain resolves a workspace-relative path and refuses anything that lands
// outside the workspace. Diff content is reachable over HTTP, so an unchecked
// path here would be an arbitrary-file read rather than a mere bad request.
// Symlinks are resolved before the check because a link inside the workspace
// may still point out of it; a path that doesn't exist (a deleted file) can't
// be resolved and is judged on its cleaned form alone.
func (m *Manager) contain(rel string) (string, error) {
	outside := func(p, root string) bool {
		return p != root && !strings.HasPrefix(p+string(filepath.Separator), root+string(filepath.Separator))
	}
	abs := filepath.Join(m.workspace, rel)
	if outside(abs, m.workspace) {
		return "", fmt.Errorf("path outside workspace: %s", rel)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		root, rerr := filepath.EvalSymlinks(m.workspace)
		if rerr != nil {
			root = m.workspace
		}
		if outside(resolved, root) {
			return "", fmt.Errorf("path outside workspace: %s", rel)
		}
	}
	return abs, nil
}

// FileDiff returns the content of rel as captured in checkpoint n and as it
// stands in the working tree now, so a caller can render what changed instead
// of only naming the file. An empty before with a non-empty after means the
// file was created after the checkpoint; the reverse means it was deleted.
//
// This is deliberately per-file: Entry.Files is a whole-workspace manifest, far
// too large to attach to a listing, and content is only ever needed for the one
// file a reader opened.
func (m *Manager) FileDiff(n int, rel string) (before, after string, err error) {
	e, ok := m.find(n)
	if !ok {
		return "", "", fmt.Errorf("no checkpoint %d", n)
	}
	abs, err := m.contain(rel)
	if err != nil {
		return "", "", err
	}
	hash, recorded := e.Files[rel]
	if recorded {
		if hash == "" {
			return "", "", fmt.Errorf("%s was too large to capture in checkpoint %d", rel, e.ID)
		}
		data, rerr := os.ReadFile(m.blobPath(hash))
		if rerr != nil {
			return "", "", fmt.Errorf("read blob: %w", rerr)
		}
		before = string(data)
	}
	// Absent now means deleted since the checkpoint — a difference to report,
	// not a failure.
	data, rerr := os.ReadFile(abs)
	if rerr != nil && !os.IsNotExist(rerr) {
		return "", "", rerr
	}
	// Neither in the checkpoint nor on disk: the caller named a path that has
	// no history and no present, which is a mistake rather than an empty diff.
	// Reporting it as one would render as an unchanged file and hide the typo.
	if !recorded && rerr != nil {
		return "", "", fmt.Errorf("no such file in checkpoint %d or the workspace: %s", e.ID, rel)
	}
	if rerr == nil {
		after = string(data)
	}
	return before, after, nil
}

// Rollback restores the workspace to checkpoint n. A snapshot of the current
// state is taken first, so the undo can itself be undone. Returns the restored
// checkpoint (whose Turn tells the caller where to truncate the transcript)
// and the paths that changed.
func (m *Manager) Rollback(n int) (Entry, []Change, error) {
	changes, e, err := m.Diff(n)
	if err != nil {
		return Entry{}, nil, err
	}
	if _, err := m.Snapshot("rollback", fmt.Sprintf("before rollback to #%d", e.ID)); err != nil {
		return Entry{}, nil, fmt.Errorf("pre-rollback snapshot: %w", err)
	}
	for _, c := range changes {
		abs := filepath.Join(m.workspace, c.Path)
		if c.Status == "created" {
			if err := os.Remove(abs); err != nil {
				return Entry{}, nil, err
			}
			continue
		}
		if err := m.restore(abs, e.Files[c.Path]); err != nil {
			return Entry{}, nil, err
		}
	}
	return e, changes, nil
}

// RollbackFile restores a single path from checkpoint n, leaving the rest of
// the workspace untouched.
func (m *Manager) RollbackFile(n int, rel string) error {
	e, ok := m.find(n)
	if !ok {
		return fmt.Errorf("no checkpoint %d", n)
	}
	hash, recorded := e.Files[rel]
	if !recorded {
		// The file did not exist at that point; undoing means removing it.
		err := os.Remove(filepath.Join(m.workspace, rel))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if hash == "" {
		return fmt.Errorf("%s was too large to capture in checkpoint %d", rel, e.ID)
	}
	return m.restore(filepath.Join(m.workspace, rel), hash)
}

func (m *Manager) restore(abs, hash string) error {
	if hash == "" {
		return nil
	}
	data, err := os.ReadFile(m.blobPath(hash))
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.cache, strings.TrimPrefix(abs, m.workspace+string(filepath.Separator)))
	m.mu.Unlock()
	return nil
}
