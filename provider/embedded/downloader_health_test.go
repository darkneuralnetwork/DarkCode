package embedded

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSharedLibsHealthy covers the corrupt-install self-heal: the regression
// was a zero-byte libllama-common.so.0 (broken symlink extraction) that made
// llama-server die with "file too short", which the old ANY-.so check treated
// as healthy so it never re-downloaded.
func TestSharedLibsHealthy(t *testing.T) {
	write := func(t *testing.T, dir, name string, size int) {
		t.Helper()
		data := make([]byte, size)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("healthy non-empty libs", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "libllama.so.0.0.9935", 4096)
		write(t, dir, "libggml-base.so.0.15.3", 2048)
		if !sharedLibsHealthy(dir) {
			t.Fatal("non-empty libraries should be reported healthy")
		}
	})

	t.Run("zero-byte lib is corrupt", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "libllama.so.0.0.9935", 4096) // a good one exists...
		write(t, dir, "libllama-common.so.0", 0)    // ...but this is the broken symlink
		if sharedLibsHealthy(dir) {
			t.Fatal("a zero-byte library must make the install unhealthy (triggers re-download)")
		}
	})

	t.Run("no libraries at all", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "llama-server", 1000) // exe present but no libs
		if sharedLibsHealthy(dir) {
			t.Fatal("a dir with no shared libraries is not healthy")
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		if sharedLibsHealthy(filepath.Join(t.TempDir(), "does-not-exist")) {
			t.Fatal("a missing dir is not healthy")
		}
	})
}
