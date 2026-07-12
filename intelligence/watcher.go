package intelligence

// watcher.go — file watcher for incremental re-indexing.
//
// The spec requires "incrementally updated" project intelligence. Rather than
// pull in a filesystem-watch dependency (fsnotify), this implements a
// debounced polling watcher: it stat-walks the tree on an interval and
// triggers a re-scan only when a .go file's mtime changed. This keeps the
// index fresh without a CGo/syscall dependency and without re-scanning on
// every keystroke.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileWatcher polls the workspace for changed Go files and invokes a callback.
type FileWatcher struct {
	root     string
	interval time.Duration
	mu       sync.Mutex
	mtimes   map[string]time.Time
	stop     chan struct{}
OnChange  func(changed []string)
}

// NewFileWatcher creates a watcher for `root` polling every `interval`.
func NewFileWatcher(root string, interval time.Duration) *FileWatcher {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &FileWatcher{
		root:     root,
		interval: interval,
		mtimes:   map[string]time.Time{},
		stop:     make(chan struct{}),
	}
}

// Start begins polling in the background. It calls OnChange with the list of
// changed .go files whenever any mtime advances. Cancel via ctx or Stop().
func (w *FileWatcher) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stop:
				return
			case <-t.C:
				changed := w.scan()
				if len(changed) > 0 && w.OnChange != nil {
					w.OnChange(changed)
				}
			}
		}
	}()
}

// Stop halts the watcher.
func (w *FileWatcher) Stop() {
	select {
	case <-w.stop:
		// already closed
	default:
		close(w.stop)
	}
}

// scan walks the root and records mtimes, returning paths that changed
// (new or modified) since the last scan.
func (w *FileWatcher) scan() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var changed []string
	_ = filepath.Walk(w.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.Contains(path, "/vendor/") {
			return nil
		}
		prev, seen := w.mtimes[path]
		mt := info.ModTime()
		if !seen || !mt.Equal(prev) {
			changed = append(changed, path)
		}
		w.mtimes[path] = mt
		return nil
	})
	return changed
}

// Snapshot returns the current file→mtime map (for debugging / stats).
func (w *FileWatcher) Snapshot() map[string]time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]time.Time, len(w.mtimes))
	for k, v := range w.mtimes {
		out[k] = v
	}
	return out
}
