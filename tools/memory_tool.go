package tools

import (
	"context"
	"fmt"
	"github.com/darkcode/internal/strutil"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
)

// MemoryTool wraps the memory store for use as an agent tool.
//
// It can write to two backends:
//   - Store:  the legacy simple key/value store (memory.json)
//   - System: the 4-tier memory system's semantic tier, which is what the
//     GUI's "4-Tier Memory → Semantic" tab displays.
//
// When a System is attached, "add" actions are routed to the semantic tier so
// they surface in the UI; otherwise they fall back to the legacy Store.
type MemoryTool struct {
	Store  *memory.Store
	System *memory.System
}

// NewMemoryTool creates a tool backed by the legacy store only.
func NewMemoryTool(store *memory.Store) *MemoryTool {
	return &MemoryTool{Store: store}
}

// NewSemanticMemoryTool creates a tool that writes to the 4-tier system's
// semantic tier (preferred) and falls back to the legacy store for
// list/search when the system has no semantic entries.
func NewSemanticMemoryTool(store *memory.Store, sys *memory.System) *MemoryTool {
	return &MemoryTool{Store: store, System: sys}
}

func (t *MemoryTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	action, _ := args["action"].(string)
	target, _ := args["target"].(string) // "user" or "memory"
	content, _ := args["content"].(string)
	oldText, _ := args["old_text"].(string)

	if target == "" {
		target = "memory"
	}

	switch action {
	case "add":
		if content == "" {
			return &ToolResult{Name: "memory", Success: false, Error: "content is required for add action"}
		}
		// Architectural decisions go to their own persisted store, not the
		// semantic tier, so they're distinguishable from general notes.
		if target == "architecture" {
			if t.System == nil {
				return &ToolResult{Name: "memory", Success: false, Error: "architecture memory unavailable (no memory system)"}
			}
			title := semanticNoteKey(content)
			t.System.ArchitectureAddDecision(title, content)
			return &ToolResult{
				Name:    "memory",
				Success: true,
				Output:  fmt.Sprintf("Recorded architecture decision [%s]: %s", title, strutil.Truncate(content, 120)),
			}
		}
		// Prefer the semantic tier (shown in the GUI) when available.
		if t.System != nil {
			key := "note:" + semanticNoteKey(content)
			category := target
			if category == "" {
				category = "memory"
			}
			if err := t.System.SemanticAdd(key, content, category, []string{"note", category}); err != nil {
				return &ToolResult{Name: "memory", Success: false, Error: err.Error()}
			}
			return &ToolResult{
				Name:    "memory",
				Success: true,
				Output:  fmt.Sprintf("Saved semantic memory: [%s] %s", category, strutil.Truncate(content, 120)),
			}
		}
		if t.Store == nil {
			return &ToolResult{Name: "memory", Success: false, Error: "no memory store available"}
		}
		entry, err := t.Store.Add(target, content)
		if err != nil {
			return &ToolResult{Name: "memory", Success: false, Error: err.Error()}
		}
		return &ToolResult{
			Name:    "memory",
			Success: true,
			Output:  fmt.Sprintf("Saved memory entry: [%s] %s: %s", entry.Category, entry.ID, entry.Content),
		}

	case "replace":
		if oldText == "" || content == "" {
			return &ToolResult{Name: "memory", Success: false, Error: "old_text and content are required for replace action"}
		}
		if t.Store == nil {
			return &ToolResult{Name: "memory", Success: false, Error: "replace is only supported on the legacy store"}
		}
		if err := t.Store.Replace(oldText, content); err != nil {
			return &ToolResult{Name: "memory", Success: false, Error: err.Error()}
		}
		return &ToolResult{Name: "memory", Success: true, Output: "Memory entry updated."}

	case "remove":
		if oldText == "" {
			return &ToolResult{Name: "memory", Success: false, Error: "old_text is required for remove action"}
		}
		if t.Store == nil {
			return &ToolResult{Name: "memory", Success: false, Error: "remove is only supported on the legacy store"}
		}
		if err := t.Store.Remove(oldText); err != nil {
			return &ToolResult{Name: "memory", Success: false, Error: err.Error()}
		}
		return &ToolResult{Name: "memory", Success: true, Output: "Memory entry removed."}

	case "list":
		if target == "architecture" {
			if t.System == nil {
				return &ToolResult{Name: "memory", Success: false, Error: "architecture memory unavailable (no memory system)"}
			}
			decisions := t.System.ArchitectureDecisions()
			if len(decisions) == 0 {
				return &ToolResult{Name: "memory", Success: true, Output: "No architecture decisions recorded."}
			}
			var sb strings.Builder
			for title, decision := range decisions {
				sb.WriteString(fmt.Sprintf("[%s] %s\n", title, decision))
			}
			return &ToolResult{Name: "memory", Success: true, Output: sb.String()}
		}
		// Prefer semantic entries from the system (what the GUI shows).
		if t.System != nil {
			entries := t.System.SemanticAll()
			if len(entries) > 0 {
				return &ToolResult{
					Name:    "memory",
					Success: true,
					Output:  formatSemanticEntries(entries),
				}
			}
		}
		if t.Store != nil {
			return &ToolResult{
				Name:    "memory",
				Success: true,
				Output:  memory.FormatEntries(t.Store.List(target)),
			}
		}
		return &ToolResult{Name: "memory", Success: true, Output: "No memory entries stored."}

	case "search":
		query, _ := args["query"].(string)
		if query == "" {
			return &ToolResult{Name: "memory", Success: false, Error: "query is required for search action"}
		}
		// Search the semantic tier first.
		if t.System != nil {
			entries := t.System.SemanticSearch(query)
			if len(entries) > 0 {
				return &ToolResult{
					Name:    "memory",
					Success: true,
					Output:  formatSemanticEntries(entries),
				}
			}
		}
		if t.Store != nil {
			return &ToolResult{
				Name:    "memory",
				Success: true,
				Output:  memory.FormatEntries(t.Store.Search(query)),
			}
		}
		return &ToolResult{Name: "memory", Success: true, Output: "No matching memory entries."}

	case "kg":
		// Knowledge-graph query. With a query term, returns the concept word's
		// related concepts (weighted co-occurrence relations). Without one,
		// returns KG stats. Lets the agent explore word relations at runtime.
		if t.System == nil {
			return &ToolResult{Name: "memory", Success: false, Error: "knowledge graph unavailable (no memory system)"}
		}
		kg := t.System.KG()
		if kg == nil {
			return &ToolResult{Name: "memory", Success: false, Error: "knowledge graph not initialized"}
		}
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			nodes, edges := kg.Stats()
			return &ToolResult{
				Name: "memory", Success: true,
				Output: fmt.Sprintf("Knowledge graph: %d nodes, %d edges.", nodes, edges),
			}
		}
		relsIface := kg.ConceptRelations(query)
		rels, ok := relsIface.([]memory.ConceptRelation)
		if !ok || len(rels) == 0 {
			return &ToolResult{Name: "memory", Success: true, Output: fmt.Sprintf("No concept relations found for %q.", query)}
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Concept relations for %q:\n", query))
		for _, r := range rels {
			sb.WriteString(fmt.Sprintf("  - %s (%s, weight: %.0f)\n", r.Label, r.Relation, r.Weight))
		}
		return &ToolResult{Name: "memory", Success: true, Output: sb.String()}

	default:
		return &ToolResult{Name: "memory", Success: false,
			Error: fmt.Sprintf("unknown action: %s (use add, replace, remove, list, search, or kg)", action)}
	}
}

// formatSemanticEntries renders semantic entries as a readable block.
func formatSemanticEntries(entries []*core.SemanticEntry) string {
	if len(entries) == 0 {
		return "No semantic memory entries found."
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", e.Category, e.Key, e.Content))
	}
	return sb.String()
}

// semanticNoteKey derives a stable key from the content so re-saving a
// similar note updates rather than duplicates.
func semanticNoteKey(content string) string {
	s := strings.ToLower(strings.TrimSpace(content))
	s = strings.ReplaceAll(s, " ", "_")
	for _, ch := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r"} {
		s = strings.ReplaceAll(s, ch, "-")
	}
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

func (t *MemoryTool) Schema() string {
	return `{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["add", "replace", "remove", "list", "search", "kg"],
				"description": "Memory operation. 'add' saves a durable semantic note. 'search' does keyword recall. 'kg' queries the knowledge graph: pass query=<word> to get that concept's related words (weighted co-occurrence), or omit query for KG stats."
			},
			"target": {
				"type": "string",
				"enum": ["user", "memory", "architecture"],
				"description": "Category: 'user' for user profile facts, 'memory' for general notes, 'architecture' for codebase design decisions (component map, dependency notes, constraints) — supports 'add' and 'list' actions"
			},
			"content": {
				"type": "string",
				"description": "Content to add or replace with"
			},
			"old_text": {
				"type": "string",
				"description": "Substring identifying the entry to replace or remove"
			},
			"query": {
				"type": "string",
				"description": "Search query (for search action)"
			}
		},
		"required": ["action"]
	}`
}
