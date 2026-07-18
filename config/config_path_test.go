package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathPrefersHomeOnFreshInstall(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	got := ConfigPath()
	want := filepath.Join(tmpHome, ".darkcode", "config.json")
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigPathMigratesFromCWD(t *testing.T) {
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
