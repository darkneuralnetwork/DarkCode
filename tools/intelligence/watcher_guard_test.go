package intelligence

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatcherSurvivesAPanickingCallback. The watcher's callback parses whatever
// turned up in the workspace, and a panic on a goroutine cannot be recovered by
// whoever started it — one malformed file would take the whole process down
// rather than losing an index update.
//
// Without the guard this test does not fail; it crashes the test binary.
func TestWatcherSurvivesAPanickingCallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewFileWatcher(dir, 10*time.Millisecond)
	panicked := make(chan struct{}, 1)
	w.OnChange = func([]string) {
		select {
		case panicked <- struct{}{}:
		default:
		}
		panic("malformed input")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Touch the file so the watcher sees a change.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(src, []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never invoked the callback; the test proves nothing")
	}

	// The process is still here, which is the whole assertion. Give the
	// watcher another tick to confirm it did not die with the panic.
	time.Sleep(50 * time.Millisecond)
}
