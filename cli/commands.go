package cli

import (
	"sort"

	"github.com/darkcode/cli/tui"
)

// cmdInfo describes one slash command for the searchable command palette
// (/help) and the tab-completer. This registry is the single source of truth so
// the palette, completions, and docs never drift apart.
type cmdInfo struct {
	Name     string // canonical name, e.g. "/model"
	Category string // grouping shown in the palette
	Summary  string // one-line description
}

// commandRegistry lists every user-facing slash command, grouped by category.
// Keep entries ordered by category then name; the palette sorts within a
// category. Adding a command here surfaces it in /help and tab-completion.
var commandRegistry = []cmdInfo{
	// Session
	{"/help", "Session", "Search & run any command (this palette)"},
	{"/new", "Session", "Start a fresh chat — clears context (keeps durable memory)"},
	{"/status", "Session", "Show the kernel/router status"},
	{"/config", "Session", "Show the current configuration"},
	{"/quit", "Session", "Exit darkcode"},

	// Chat & modes
	{"/chatmode", "Chat & Modes", "Chat / Build / Build+Loop (tools & auto-task policy)"},
	{"/brain", "Chat & Modes", "Routing brain: auto (local-first) / local (offline) / cloud"},
	{"/mode", "Chat & Modes", "Routing mode: single / escalation / consensus"},
	{"/safety", "Chat & Modes", "Approval level: strict / normal / relaxed"},
	{"/profile", "Chat & Modes", "Execution profile: auto / sequential / parallel"},

	// Models & local
	{"/model", "Models & Local", "Switch the active model (selector)"},
	{"/models", "Models & Local", "List registered models"},
	{"/providers", "Models & Local", "List configured providers"},
	{"/local", "Models & Local", "Local LLM: off / on / force (offline routing)"},
	{"/memory-profile", "Models & Local", "Local model context/RAM: lean / balanced / max"},
	{"/compressor", "Models & Local", "Set the context-compression model"},

	// Knowledge & memory
	{"/ingest", "Knowledge & Memory", "Teach the system a file, directory, URL, or text"},
	{"/memory", "Knowledge & Memory", "Show the memory summary"},
	{"/skills", "Knowledge & Memory", "List learned procedural skills"},
	{"/episodes", "Knowledge & Memory", "List episodic memory (past tasks)"},
	{"/know", "Knowledge & Memory", "Browse the knowledge graph"},
	{"/learning", "Knowledge & Memory", "Show learning-engine feedback"},
	{"/audit", "Knowledge & Memory", "Show the action audit trail"},

	// Project
	{"/project", "Project", "List / manage projects"},
	{"/plan", "Project", "Show the active project's implementation plan"},
	{"/workflow", "Project", "Show the active project's task workflow"},

	// Tools & system
	{"/tools", "Tools & System", "List / inspect available tools"},
	{"/plugins", "Tools & System", "List loaded plugins"},
	{"/sandbox", "Tools & System", "Show the security sandbox status"},
	{"/pipeline", "Tools & System", "Show the verification pipeline"},
	{"/permissions", "Tools & System", "Show permission-gate settings"},

	// Observability
	{"/monitor", "Observability", "Open the live monitoring dashboard"},
	{"/usage", "Observability", "Token/cost usage report"},
	{"/history", "Observability", "Show full request history"},
	{"/stats", "Observability", "Show hardware stats"},
	{"/events", "Observability", "Show the event stream"},
	{"/log", "Observability", "Replay the activity/trace log"},
}

// commandSelectorItems builds the palette entries, grouped by category (a
// category order is imposed, and names are sorted within each category). The
// Description carries the category tag + summary so the fuzzy filter matches on
// command name, category, and summary alike.
func commandSelectorItems() []tui.SelectorItem {
	catOrder := []string{
		"Session", "Chat & Modes", "Models & Local",
		"Knowledge & Memory", "Project", "Tools & System", "Observability",
	}
	rank := make(map[string]int, len(catOrder))
	for i, c := range catOrder {
		rank[c] = i
	}
	sorted := make([]cmdInfo, len(commandRegistry))
	copy(sorted, commandRegistry)
	sort.SliceStable(sorted, func(i, j int) bool {
		if rank[sorted[i].Category] != rank[sorted[j].Category] {
			return rank[sorted[i].Category] < rank[sorted[j].Category]
		}
		return sorted[i].Name < sorted[j].Name
	})
	items := make([]tui.SelectorItem, 0, len(sorted))
	for _, c := range sorted {
		items = append(items, tui.SelectorItem{
			Title:       c.Name,
			Description: c.Category + " · " + c.Summary,
			Value:       c.Name,
		})
	}
	return items
}
