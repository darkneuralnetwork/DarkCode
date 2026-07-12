package server

import (
	"net/http"

	"github.com/darkcode/intelligence"
)

func (s *Server) handleIntelligenceSummary(w http.ResponseWriter, r *http.Request) {
	s.wsMu.RLock()
	workspace := s.activeWorkspace
	s.wsMu.RUnlock()

	if workspace == "" {
		workspace = "." // fallback
	}

	index := intelligence.NewProjectIndex(workspace)
	_ = index.ScanWorkspace() // scan synchronously for now

	writeJSON(w, http.StatusOK, index.Stats())
}
