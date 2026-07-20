package cli

import "testing"

// TestCommandRegistryCoversPhases guards CLI↔GUI parity: every command the
// 5-phase plan surfaces in the CLI must be registered so it appears in the
// searchable /help palette and tab-completion.
func TestCommandRegistryCoversPhases(t *testing.T) {
	have := map[string]cmdInfo{}
	for _, c := range commandRegistry {
		if _, dup := have[c.Name]; dup {
			t.Errorf("duplicate command in registry: %s", c.Name)
		}
		have[c.Name] = c
	}
	// Phase-specific + core commands that must exist.
	required := []string{
		"/help",           // rebuilt palette
		"/chatmode",       // Phase 5: Chat/Build/Loop
		"/brain",          // Phase 1: local/cloud/auto
		"/memory-profile", // Phase 1: lean/balanced/max
		"/ingest",         // Phase 3: knowledge ingestion
		"/local", "/model", "/mode", "/safety", "/new",
	}
	for _, name := range required {
		if _, ok := have[name]; !ok {
			t.Errorf("required command %q missing from registry", name)
		}
	}
}

func TestCommandSelectorItemsGroupedAndComplete(t *testing.T) {
	items := commandSelectorItems()
	if len(items) != len(commandRegistry) {
		t.Fatalf("selector items (%d) should match registry (%d)", len(items), len(commandRegistry))
	}
	// Every item must carry a category tag in its description (for grouping +
	// fuzzy search) and a non-empty value to run.
	for _, it := range items {
		if it.Value == "" || it.Title == "" {
			t.Errorf("selector item missing title/value: %+v", it)
		}
		if it.Description == "" {
			t.Errorf("selector item %q missing description", it.Title)
		}
	}
	// Session commands should sort before Observability ones (category order).
	posHelp, posMonitor := -1, -1
	for i, it := range items {
		if it.Value == "/help" {
			posHelp = i
		}
		if it.Value == "/monitor" {
			posMonitor = i
		}
	}
	if posHelp == -1 || posMonitor == -1 || posHelp >= posMonitor {
		t.Errorf("category ordering wrong: /help at %d should precede /monitor at %d", posHelp, posMonitor)
	}
}
