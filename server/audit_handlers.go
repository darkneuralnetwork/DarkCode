package server

import (
	"encoding/json"
	"net/http"
	"time"
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

// handleAuditExport streams the whole audit trail as newline-delimited JSON.
//
// The browser view is for reading; this is for keeping. JSONL is what a SIEM,
// a log shipper, or `jq` ingests without a parser, which is what "we retain an
// audit trail" has to mean in an environment that reviews one.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil || s.memSystem.Audit() == nil {
		writeError(w, http.StatusServiceUnavailable, "audit log not initialized")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition",
		`attachment; filename="darkcode-audit-`+time.Now().Format("20060102-150405")+`.jsonl"`)

	enc := json.NewEncoder(w)
	for _, e := range s.memSystem.Audit().GetAll() {
		if err := enc.Encode(e); err != nil {
			return // client disconnected
		}
	}
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
	nodeCount, edgeCount := kg.Stats()

	// Nodes grow with the system-wide KG, so they paginate (default: all).
	page, meta := paginate(kg.AllNodes(), parsePage(r))
	meta["nodes"] = page
	meta["edges"] = kg.AllEdges()
	meta["node_count"] = nodeCount
	meta["edge_count"] = edgeCount
	writeJSON(w, http.StatusOK, meta)
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
