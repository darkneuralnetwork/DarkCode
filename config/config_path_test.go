package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathPrefersHomeOnFreshInstall(t *testing.T) {
	// These cover the resolution chain itself, so the explicit override the
	// package TestMain sets has to be out of the way.
	t.Setenv("DARKCODE_CONFIG", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	got := ConfigPath()
	want := filepath.Join(tmpHome, ".darkcode", "config.json")
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigPathMigratesFromCWD(t *testing.T) {
	// These cover the resolution chain itself, so the explicit override the
	// package TestMain sets has to be out of the way.
	t.Setenv("DARKCODE_CONFIG", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	tmpCwd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpCwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	legacyPath := filepath.Join(tmpCwd, ".config")
	if err := os.WriteFile(legacyPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ConfigPath()
	if got != legacyPath {
		t.Fatalf("ConfigPath() = %q, want legacy %q (migration fallback)", got, legacyPath)
	}
}

func TestConfigPathPrefersHomeWhenBothExist(t *testing.T) {
	// These cover the resolution chain itself, so the explicit override the
	// package TestMain sets has to be out of the way.
	t.Setenv("DARKCODE_CONFIG", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	tmpCwd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpCwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	homePath := filepath.Join(tmpHome, ".darkcode", "config.json")
	if err := os.MkdirAll(filepath.Dir(homePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homePath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(tmpCwd, ".config")
	if err := os.WriteFile(legacyPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	got := ConfigPath()
	if got != homePath {
		t.Fatalf("ConfigPath() = %q, want home %q (system-wide wins once it exists)", got, homePath)
	}
}

// TestConfigPathOverrideWins pins the escape hatch the test suites depend on.
// Without it every test that saves a config edits the developer's own.
func TestConfigPathOverrideWins(t *testing.T) {
	want := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv("DARKCODE_CONFIG", want)

	if got := ConfigPath(); got != want {
		t.Errorf("ConfigPath() = %q, want the override %q", got, want)
	}

	// Whitespace-only is not a path; it must fall through to the normal chain
	// rather than resolving to "".
	t.Setenv("DARKCODE_CONFIG", "   ")
	if got := ConfigPath(); got == "" || got == "   " {
		t.Errorf("a blank override produced %q", got)
	}
}
