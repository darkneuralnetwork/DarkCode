package server

import (
	"context"
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

	writeJSON(w, http.StatusOK, s.projectIndex(workspace).Stats())
}

// projectIndex returns the long-lived index for workspace, building and
// scanning it on first use and keeping it fresh from then on.
//
// This handler used to construct a ProjectIndex per request and walk the entire
// tree synchronously before answering — so every call re-parsed the whole
// repository, and the FileWatcher the index carries was pointless because the
// index it would have updated was discarded microseconds later. Holding one
// index per workspace makes the watcher meaningful and turns the second and
// subsequent calls into a map read.
func (s *Server) projectIndex(workspace string) *intelligence.ProjectIndex {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	if idx, ok := s.indexes[workspace]; ok {
		return idx
	}
	idx := intelligence.NewProjectIndex(workspace)
	_ = idx.ScanWorkspace()
	// Background refresh is tied to the server's lifetime, not the request's;
	// stopIndexes tears them down on shutdown.
	idx.StartWatching(context.Background())

	if s.indexes == nil {
		s.indexes = map[string]*intelligence.ProjectIndex{}
	}
	s.indexes[workspace] = idx
	return idx
}

// stopIndexes halts every workspace watcher. Called on shutdown so the
// polling goroutines do not outlive the server.
func (s *Server) stopIndexes() {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for _, idx := range s.indexes {
		idx.StopWatching()
	}
	s.indexes = nil
}
