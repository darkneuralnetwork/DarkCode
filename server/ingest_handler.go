package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/darkcode/ingest"
)

// handleIngest teaches the system from an external source (file, directory/repo,
// http(s) URL, or raw text): the content is chunked, embedded into semantic
// memory, and — for code directories — indexed into the knowledge graph, so it
// can be recalled later, including offline via the local model.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system unavailable")
		return
	}
	var req struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}

	// Ingestion of a large repo/URL can take a while; bound it.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	ing := ingest.New(s.memSystem, s.memSystem.KG())
	st, err := ing.Ingest(ctx, req.Source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.emitter != nil {
		s.emitter.EmitTaskUpdate("ingest", "done", req.Source)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"sources":  st.Sources,
		"chunks":   st.Chunks,
		"kg_nodes": st.KGNodes,
		"skipped":  st.Skipped,
		"errors":   st.Errors,
	})
}
