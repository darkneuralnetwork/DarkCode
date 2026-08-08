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
	//
	// Paged, like the per-tier endpoints beside it. This one served every entry
	// of every tier and the browser sliced to fifty after downloading the lot —
	// invisible at a few dozen entries, and megabytes of JSON to paint one
	// screen once episodic memory reaches its cap. The counts travel alongside,
	// because a paged response with no total is indistinguishable from a nearly
	// empty memory.
	p := parsePage(r)
	if p.limit == 0 {
		p.limit = memoryPageDefault
	}
	episodic := s.memSystem.EpisodicGet()
	semantic := s.memSystem.SemanticAll()
	procedural := s.memSystem.ProceduralAll()
	stm := s.memSystem.STMGet()

	epPage, _ := paginate(episodic, p)
	semPage, _ := paginate(semantic, p)
	procPage, _ := paginate(procedural, p)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": stm, // this conversation; bounded by compaction already
		"episodic":     epPage,
		"semantic":     semPage,
		"procedural":   procPage,
		"counts": map[string]int{
			"conversation": len(stm),
			"episodic":     len(episodic),
			"semantic":     len(semantic),
			"procedural":   len(procedural),
		},
		"limit":  p.limit,
		"offset": p.offset,
	})
}

// memoryPageDefault matches what the browser renders. Asking for more is
// allowed; asking for nothing no longer means "everything".
const memoryPageDefault = 50

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
