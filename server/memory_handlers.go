package server

import (
	"net/http"
)

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}

	// Keys name the tier they carry. They used to be aspirational — procedural
	// skills were served as "user" and semantic facts as "architecture", so the
	// interface labelled a list of learned procedures "User Memory", which is
	// where nobody would look for them. Two further keys held a sentence
	// explaining they were unimplemented, which reads to any consumer as a tier
	// containing one string.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": s.memSystem.STMGet(),        // short-term: this conversation
		"episodic":     s.memSystem.EpisodicGet(),   // past tasks and their outcomes
		"semantic":     s.memSystem.SemanticAll(),   // durable facts
		"procedural":   s.memSystem.ProceduralAll(), // skills, learned and imported
	})
}

// handleShortTermMemory returns short-term memory (working context).
func (s *Server) handleShortTermMemory(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	msgs := s.memSystem.STMGet()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messages": msgs,
		"count":    len(msgs),
	})
}

// handleEpisodicMemory returns episodic memory (task history).
func (s *Server) handleEpisodicMemory(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	writePage(w, "episodes", s.memSystem.EpisodicGet(), parsePage(r))
}

// handleSemanticMemory returns semantic memory (facts).
func (s *Server) handleSemanticMemory(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	writePage(w, "facts", s.memSystem.SemanticAll(), parsePage(r))
}

// handleProceduralMemory returns procedural memory (skills).
func (s *Server) handleProceduralMemory(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	writePage(w, "skills", s.memSystem.ProceduralAll(), parsePage(r))
}

// handleConfig returns the current configuration (minus secrets) or updates it.
