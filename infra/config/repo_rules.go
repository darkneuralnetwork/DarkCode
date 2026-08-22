package config

import (
	"os"
	"path/filepath"
)

// repoRulesMaxBytes bounds how much of a rules file gets injected into the
// system prompt every turn, so a large file can't eat the context budget.
const repoRulesMaxBytes = 32 * 1024

// repoRulesCandidates are checked in order at the repo root; the first one
// found wins. AGENTS.md is the cross-tool convention several other agents
// already read; .darkcode/RULES.md and CLAUDE.md are checked after so an
// existing file from either convention still gets picked up.
var repoRulesCandidates = []string{
	"AGENTS.md",
	filepath.Join(".darkcode", "RULES.md"),
	"CLAUDE.md",
}

// loadRepoRules reads the first repo-rules file found under dir, capped at
// repoRulesMaxBytes. Returns "" if none exist or dir can't be read.
func loadRepoRules(dir string) string {
	for _, name := range repoRulesCandidates {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if len(data) > repoRulesMaxBytes {
			data = data[:repoRulesMaxBytes]
		}
		return string(data)
	}
	return ""
}
