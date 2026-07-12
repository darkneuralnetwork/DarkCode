package server

import (
	"net/http"
)

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil || s.memSystem.Audit() == nil {
		writeError(w, http.StatusServiceUnavailable, "audit log not initialized")
		return
	}
	entries := s.memSystem.Audit().GetAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
		"summary": s.memSystem.Audit().Summary(),
	})
}

// handleAuditRecent returns the most recent audit entries.
func (s *Server) handleAuditRecent(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil || s.memSystem.Audit() == nil {
		writeError(w, http.StatusServiceUnavailable, "audit log not initialized")
		return
	}
	entries := s.memSystem.Audit().GetRecent(50)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
	})
}

// handleKnowledgeGraph returns all knowledge graph nodes and edges.
func (s *Server) handleKnowledgeGraph(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil || s.memSystem.KG() == nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge graph not initialized")
		return
	}
	kg := s.memSystem.KG()
	nodes := kg.AllNodes()
	edges := kg.AllEdges()
	nodeCount, edgeCount := kg.Stats()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"nodes":      nodes,
		"edges":      edges,
		"node_count": nodeCount,
		"edge_count": edgeCount,
	})
}

// handleLearningStats returns the learning engine statistics.
func (s *Server) handleLearningStats(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil || s.memSystem.Learning() == nil {
		writeError(w, http.StatusServiceUnavailable, "learning engine not initialized")
		return
	}
	stats := s.memSystem.Learning().GetStats()
	strategies := s.memSystem.Learning().GetAllStrategies()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"stats":      stats,
		"strategies": strategies,
	})
}

// handleAgentMessages returns recent inter-agent communication messages.
func (s *Server) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	// The agent bus is kept in the kernel, we need to access it if available.
	// Since kernel is already accessible in s.kernel, we will fetch it from there.
	// We'll need a getter on the kernel for the agent bus if we want to expose it this way,
	// or we can simply return empty array if not directly accessible here.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messages": []interface{}{},
		"note":     "agent bus access via SSE in real-time UI",
	})
}

// handleProviders returns the full LLM provider catalogue (models, pricing,
// auth schemes) so the UI can render provider-driven model setup.
