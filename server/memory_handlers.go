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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": s.memSystem.STMGet(),
		"session":      []interface{}{"Session memory tracks short-term context across tasks (Beta)."},
		"project":      s.memSystem.EpisodicGet(),
		"workspace":    []interface{}{"Workspace context connects project memory to file system state (Beta)."},
		"user":         s.memSystem.ProceduralAll(),
		"architecture": s.memSystem.SemanticAll(),
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
	episodes := s.memSystem.EpisodicGet()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"episodes": episodes,
		"count":    len(episodes),
	})
}

// handleSemanticMemory returns semantic memory (facts).
func (s *Server) handleSemanticMemory(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	facts := s.memSystem.SemanticAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"facts": facts,
		"count": len(facts),
	})
}

// handleProceduralMemory returns procedural memory (skills).
func (s *Server) handleProceduralMemory(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	skills := s.memSystem.ProceduralAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"skills": skills,
		"count":  len(skills),
	})
}

// handleSkills is an alias for procedural memory.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	skills := s.memSystem.ProceduralAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"skills": skills,
		"count":  len(skills),
	})
}

// handleEpisodes is an alias for episodic memory.
func (s *Server) handleEpisodes(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	episodes := s.memSystem.EpisodicGet()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"episodes": episodes,
		"count":    len(episodes),
	})
}

// handleConfig returns the current configuration (minus secrets) or updates it.
