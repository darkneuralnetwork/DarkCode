package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	entries := s.registry.AllEntries()
	tools := make([]map[string]interface{}, len(entries))
	for i, te := range entries {
		source := te.Source
		if source == "" {
			source = "builtin"
		}
		tools[i] = map[string]interface{}{
			"name":        te.Name,
			"description": te.Description,
			"category":    te.Category,
			"source":      source,
			"parameters":  te.Parameters,
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tools": tools,
		"count": len(tools),
	})
}

// handleToolExecute executes a tool directly.
func (s *Server) handleToolExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Tool    string                 `json:"tool"`
		Args    map[string]interface{} `json:"args"`
		Timeout int                    `json:"timeout"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	timeout := 30
	if req.Timeout > 0 {
		timeout = req.Timeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	result, err := s.registry.Execute(ctx, req.Tool, req.Args)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":    result.Name,
		"success": result.Success,
		"output":  result.Output,
		"error":   result.Error,
	})
}

// guiDisconnectGrace is how long the server waits after the last GUI SSE
// client disconnects before resuming CLI mode. It must exceed the browser's
// default EventSource reconnect interval (~3s) so that a tab refresh or a
// transient network drop does not falsely trigger a switch.
const guiDisconnectGrace = 5 * time.Second

// ssePingInterval is how often the SSE handler writes a heartbeat comment.
// It must stay well under guiDisconnectGrace so a broken connection is
// detected (write fails) before the resume timer is even armed.
const ssePingInterval = 2 * time.Second

// handleSSE streams events via Server-Sent Events.
