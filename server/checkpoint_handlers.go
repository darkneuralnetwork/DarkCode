package server

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// handleCheckpoints lists the recorded pre-mutation snapshots.
func (s *Server) handleCheckpoints(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil || s.kernel.Checkpoints() == nil {
		writeError(w, http.StatusServiceUnavailable, "checkpoints not initialized")
		return
	}
	entries := s.kernel.Checkpoints().List()
	// Files is the full workspace manifest — useful to the engine, far too
	// large for a listing, so it is dropped here.
	out := make([]map[string]interface{}, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]interface{}{
			"id": e.ID, "time": e.Time, "tool": e.Tool,
			"label": e.Label, "turn": e.Turn, "files": len(e.Files),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"checkpoints": out, "count": len(out)})
}

// handleCheckpointDiff reports how the working tree differs from a checkpoint.
func (s *Server) handleCheckpointDiff(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil || s.kernel.Checkpoints() == nil {
		writeError(w, http.StatusServiceUnavailable, "checkpoints not initialized")
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	changes, entry, err := s.kernel.Checkpoints().Diff(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": entry.ID, "label": entry.Label, "changes": changes,
	})
}

// handleRollback restores the workspace to a checkpoint and rewinds the
// conversation to match.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if s.kernel == nil || s.kernel.Checkpoints() == nil {
		writeError(w, http.StatusServiceUnavailable, "checkpoints not initialized")
		return
	}
	var body struct {
		ID   int    `json:"id"`
		File string `json:"file"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.File != "" {
		if err := s.kernel.Checkpoints().RollbackFile(body.ID, body.File); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"restored": []string{body.File}})
		return
	}

	entry, changes, err := s.kernel.RollbackTo(body.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": entry.ID, "turn": entry.Turn, "changes": changes, "count": len(changes),
	})
}
