package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/darkcode/memory"
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

// handleMemorySearch answers a question against memory instead of listing it.
//
// # WHY SEARCH AND NOT A LIST
//
// The Memory tab used to open by fetching every tier and the whole knowledge
// graph — 16 MB on a real repository — and then render fifty rows of it. It
// hung the browser, and paginating the dump only made a smaller dump.
//
// The deeper point is that memory here is a retrieval system. Nobody scrolls
// fourteen thousand graph nodes looking for something; they ask. So the
// interface asks, through the same hybrid retriever the agent itself uses —
// keyword, vector and knowledge-graph signals fused by reciprocal rank.
//
// That also makes the thing this project is actually proud of visible: each
// hit reports WHICH signal found it, so "the graph earns its keep" stops being
// a claim in a document and becomes something you can watch happen.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if s.memSystem == nil {
		writeError(w, http.StatusServiceUnavailable, "memory system not initialized")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		// An empty query is the tab's resting state, not an error: it wants the
		// counts so it can say how much there is to search.
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"query": "", "hits": []any{}, "counts": s.memoryCounts(),
		})
		return
	}
	limit := memoryPageDefault
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 20 {
		limit = v
	}

	hits := memory.NewHybridRetriever(s.memSystem, s.memSystem.KG()).Recall(q, limit)
	out := make([]map[string]interface{}, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]interface{}{
			"id": h.ID, "source": h.Source, "goal": h.Goal,
			"snippet": h.Snippet, "score": h.Score,
			// Which stream found it. This is the differentiator, so it is on
			// the wire rather than inferred.
			"signal":    h.Signal,
			"timestamp": h.Timestamp,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"query": q, "hits": out, "counts": s.memoryCounts(),
	})
}

// memoryCounts is how much there is, without sending any of it.
func (s *Server) memoryCounts() map[string]int {
	nodes, edges := 0, 0
	if kg := s.memSystem.KG(); kg != nil {
		nodes, edges = kg.Stats()
	}
	return map[string]int{
		"conversation": len(s.memSystem.STMGet()),
		"episodic":     len(s.memSystem.EpisodicGet()),
		"semantic":     len(s.memSystem.SemanticAll()),
		"procedural":   len(s.memSystem.ProceduralAll()),
		"graph_nodes":  nodes,
		"graph_edges":  edges,
	}
}
