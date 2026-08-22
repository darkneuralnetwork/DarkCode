package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// Plugin discovery decides which files on disk get executed as plugins. It had
// no tests, and the two failure directions are asymmetric: missing a real
// plugin is an annoyance, while running something that merely sits in the
// directory is not.

func TestIsPluginBinaryFollowsTheConvention(t *testing.T) {
	accepted := []string{
		"plugin-hello",
		"plugin-",
		"thing.plugin",
		"thing.plugin.exe",
	}
	for _, name := range accepted {
		if !isPluginBinary(name) {
			t.Errorf("%q does not match the documented convention but should", name)
		}
	}

	rejected := []string{
		"README.md",
		"notes.txt",
		"myplugin",       // no prefix, no suffix
		"plugin.json",    // a manifest, not a binary
		"a-plugin-thing", // "plugin-" is not at the start
		"thing.pluginx",
		"",
		".",
	}
	for _, name := range rejected {
		if isPluginBinary(name) {
			t.Errorf("%q was accepted as a plugin binary", name)
		}
	}
}

// TestDiscoverAllOnAMissingDirectory. No plugin directory is the normal case
// for most installs; it must not be an error.
func TestDiscoverAllOnAMissingDirectory(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "nope"))
	if err := r.DiscoverAll(); err != nil {
		t.Errorf("a missing plugin directory errored: %v", err)
	}
	if got := len(r.Plugins()); got != 0 {
		t.Errorf("found %d plugins in a directory that does not exist", got)
	}
}

func TestDiscoverAllReadsManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("one.json", `{"name":"one","version":"1.0.0"}`)
	write("two.json", `{"name":"two","version":"2.0.0"}`)

	r := NewRegistry(dir)
	if err := r.DiscoverAll(); err != nil {
		t.Fatalf("DiscoverAll: %v", err)
	}

	names := map[string]bool{}
	for _, m := range r.Plugins() {
		names[m.Name] = true
	}
	for _, want := range []string{"one", "two"} {
		if !names[want] {
			t.Errorf("manifest %q was not discovered", want)
		}
	}
}

// TestDiscoverAllSkipsWhatIsNotAManifest. One unreadable or malformed file in
// the directory must not take the rest of the plugins down with it.
func TestDiscoverAllSkipsWhatIsNotAManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("good.json", `{"name":"good","version":"1.0.0"}`)
	write("broken.json", `{not valid json`)
	write("README.md", `# not a manifest`)
	write("binary.plugin", "\x00\x01\x02")
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(dir)
	if err := r.DiscoverAll(); err != nil {
		t.Fatalf("one bad file failed the whole scan: %v", err)
	}

	got := r.Plugins()
	if len(got) != 1 {
		t.Fatalf("discovered %d plugins, want only the valid one: %+v", len(got), got)
	}
	if got[0].Name != "good" {
		t.Errorf("discovered %q, want good", got[0].Name)
	}
}

// TestDiscoverAllReplacesRatherThanAccumulates. Rescanning after a plugin is
// removed must not keep reporting it as present.
func TestDiscoverAllReplacesRatherThanAccumulates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.json")
	if err := os.WriteFile(path, []byte(`{"name":"one","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(dir)
	if err := r.DiscoverAll(); err != nil {
		t.Fatal(err)
	}
	if len(r.Plugins()) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(r.Plugins()))
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := r.DiscoverAll(); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Plugins()); got != 0 {
		t.Errorf("a removed plugin is still reported: %d remain", got)
	}
}

// TestHostShutdownIsSafeWhenEmpty. Shutdown runs on every exit, including one
// where no plugin ever loaded.
func TestHostShutdownIsSafeWhenEmpty(t *testing.T) {
	h := NewHost()
	h.Shutdown()
	h.Shutdown() // twice, since exit paths can overlap
	if got := len(h.Manifests()); got != 0 {
		t.Errorf("an empty host reports %d manifests", got)
	}
}

// TestExecuteOnAnUnloadedPluginFails rather than panicking — the tool layer
// calls this with whatever name the model produced.
func TestExecuteOnAnUnloadedPluginFails(t *testing.T) {
	h := NewHost()
	defer h.Shutdown()

	if _, err := h.Execute("/no/such/plugin", "anything", nil); err == nil {
		t.Error("executing an unloaded plugin returned no error")
	}
}

// TestLoadRejectsAMissingBinary. A manifest naming a binary that is not there
// must fail loudly at load rather than at first use.
func TestLoadRejectsAMissingBinary(t *testing.T) {
	h := NewHost()
	defer h.Shutdown()

	if err := h.Load(filepath.Join(t.TempDir(), "plugin-absent")); err == nil {
		t.Error("loading a nonexistent binary succeeded")
	}
}
