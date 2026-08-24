package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRulesFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRepoRulesPrefersAgentsMD(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, "AGENTS.md", "agents rules")
	writeRulesFile(t, dir, "CLAUDE.md", "claude rules")
	writeRulesFile(t, dir, ".darkcode/RULES.md", "darkcode rules")

	if got := loadRepoRules(dir); got != "agents rules" {
		t.Errorf("got %q, want AGENTS.md content", got)
	}
}

func TestLoadRepoRulesFallsBackToDarkcodeRules(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, ".darkcode/RULES.md", "darkcode rules")
	writeRulesFile(t, dir, "CLAUDE.md", "claude rules")

	if got := loadRepoRules(dir); got != "darkcode rules" {
		t.Errorf("got %q, want .darkcode/RULES.md content (AGENTS.md absent)", got)
	}
}

func TestLoadRepoRulesFallsBackToClaudeMD(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, "CLAUDE.md", "claude rules")

	if got := loadRepoRules(dir); got != "claude rules" {
		t.Errorf("got %q, want CLAUDE.md content", got)
	}
}

func TestLoadRepoRulesEmptyWhenNoneExist(t *testing.T) {
	dir := t.TempDir()
	if got := loadRepoRules(dir); got != "" {
		t.Errorf("got %q, want empty string when no rules file exists", got)
	}
}

func TestLoadRepoRulesCapsAt32KiB(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, "AGENTS.md", strings.Repeat("x", 40000))

	got := loadRepoRules(dir)
	if len(got) != 32*1024 {
		t.Errorf("got %d bytes, want capped to 32KiB", len(got))
	}
}
