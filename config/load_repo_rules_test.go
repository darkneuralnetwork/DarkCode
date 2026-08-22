package config

import (
	"os"
	"testing"
)

// TestLoadPopulatesRepoRulesFromCWD covers the wiring, not just the loader:
// Load() must call loadRepoRules against the working directory on every
// call, including the common case where no config.json exists yet.
func TestLoadPopulatesRepoRulesFromCWD(t *testing.T) {
	t.Setenv("DARKCODE_CONFIG", "")
	t.Setenv("HOME", t.TempDir())
	tmpCwd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpCwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	writeRulesFile(t, tmpCwd, "AGENTS.md", "always run tests before committing")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoRules != "always run tests before committing" {
		t.Errorf("Load().RepoRules = %q, want the AGENTS.md content", cfg.RepoRules)
	}
}
