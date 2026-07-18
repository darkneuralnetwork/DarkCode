package ingest

import (
	"context"
	"fmt"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
	"github.com/darkcode/tools"
)

// NewIngestTool returns an agent-callable "ingest" tool that teaches the system
// new knowledge from a file, directory/repo, URL, or raw text. Ingested content
// is chunked, embedded, and stored so it can be recalled later — including
// offline via the local model.
func NewIngestTool(mem *memory.System, kg core.KnowledgeGraphStore) *tools.ToolEntry {
	ing := New(mem, kg)
	return &tools.ToolEntry{
		Name: "ingest",
		Description: "Learn from external material so it can be recalled later (including offline). " +
			"Accepts a file path, a directory/repo path, an http(s) URL, or raw text. The content is " +
			"chunked, embedded into semantic memory, and (for code directories) indexed into the knowledge graph.",
		Parameters: tools.MustParseSchema(`{
			"type": "object",
			"properties": {
				"source": {"type": "string", "description": "A file path, directory/repo path, http(s) URL, or raw text to learn from."}
			},
			"required": ["source"]
		}`),
		Category: "memory",
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			source, _ := args["source"].(string)
			if source == "" {
				return &tools.ToolResult{Name: "ingest", Success: false, Error: "source is required"}
			}
			st, err := ing.Ingest(ctx, source)
			if err != nil {
				return &tools.ToolResult{Name: "ingest", Success: false, Error: err.Error()}
			}
			out := fmt.Sprintf("Ingested %d source(s) into %d memory chunk(s)", st.Sources, st.Chunks)
			if st.KGNodes > 0 {
				out += fmt.Sprintf(", indexed %d code nodes", st.KGNodes)
			}
			if st.Skipped > 0 {
				out += fmt.Sprintf(", skipped %d file(s)", st.Skipped)
			}
			if len(st.Errors) > 0 {
				out += fmt.Sprintf(" (%d error(s): %v)", len(st.Errors), st.Errors)
			}
			return &tools.ToolResult{Name: "ingest", Success: true, Output: out}
		},
	}
}
