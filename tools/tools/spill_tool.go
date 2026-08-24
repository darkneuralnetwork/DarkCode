package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/darkcode/tools/spill"
)

// read_result is the other half of spilling. Without it, offloading a large
// tool result would be exactly as lossy as the truncation it replaces — the
// model would see a preview and have no way to reach the rest.
//
// It is read-only and reads nothing but this store, whose ids are hashes the
// agent produced itself, so it cannot be steered into the filesystem.

// RegisterSpillTool registers read_result against store. No-op when store is
// nil, so a build without spilling simply does not offer the tool rather than
// offering one that always fails.
func RegisterSpillTool(r *Registry, store *spill.Store) {
	if r == nil || store == nil {
		return
	}
	r.Register(&ToolEntry{
		Name: "read_result",
		Description: "Read any byte range of a large tool result that was offloaded from the conversation. " +
			"Use the id shown in a '[result <id> — N bytes]' header. Page through long output with offset/limit " +
			"instead of re-running the tool that produced it.",
		Category: "search",
		Source:   "builtin",
		ReadOnly: true,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id":     {"type": "string",  "description": "The result id from the '[result <id> ...]' header."},
				"offset": {"type": "integer", "description": "Byte offset to start at. Default 0."},
				"limit":  {"type": "integer", "description": "Maximum bytes to return. Default 4000."}
			},
			"required": ["id"]
		}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			id, _ := args["id"].(string)
			if id == "" {
				return &ToolResult{Name: "read_result", Success: false, Error: "id is required"}
			}
			offset := intArg(args, "offset", 0)
			limit := intArg(args, "limit", 4000)

			chunk, err := store.Get(id, offset, limit)
			if err != nil {
				return &ToolResult{Name: "read_result", Success: false, Error: err.Error()}
			}
			if chunk == "" {
				return &ToolResult{
					Name: "read_result", Success: true,
					Output: fmt.Sprintf("[offset %d is at or past the end of result %s]", offset, id),
				}
			}

			header := fmt.Sprintf("[result %s, bytes %d–%d", id, offset, offset+len(chunk))
			if ref, ok := store.Stat(id); ok {
				header += fmt.Sprintf(" of %d", ref.Bytes)
				if next := offset + len(chunk); next < ref.Bytes {
					header += fmt.Sprintf("; continue at offset %d", next)
				} else {
					header += "; end of result"
				}
			}
			header += "]\n\n"

			return &ToolResult{Name: "read_result", Success: true, Output: header + chunk}
		},
	})
}

// intArg reads a numeric argument, tolerating the float64 that JSON decoding
// produces for every number.
func intArg(args map[string]interface{}, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}
