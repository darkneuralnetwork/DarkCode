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
// graphPageDefault bounds an unasked-for graph read. Chosen to be enough to
// show shape and far short of enough to stall a browser.
const graphPageDefault = 200

func (s *Server) handleKnowledgeGraph(w http.ResponseWriter, r *http.Request) {
	if s.memSystem == nil || s.memSystem.KG() == nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge graph not initialized")
		return
	}
	kg := s.memSystem.KG()
	nodeCount, edgeCount := kg.Stats()

	// Nodes and edges both paginate, and both default to a page rather than to
	// everything.
	//
	// This used to default to "all" for nodes and send EVERY edge with no limit
	// at all. On a real repository that is a 16 MB response — a graph of ~14k
	// nodes and ~40k edges — which the browser then has to parse and lay out.
	// The Memory tab hung on open, and the cause was not the tab: it was this
	// handler answering "show me the graph" with the whole graph.
	//
	// Nobody reads 40,000 edges. A caller that genuinely wants them can page
	// through; the interface asks what it can show.
	p := parsePage(r)
	if p.limit == 0 {
		p.limit = graphPageDefault
	}
	nodes, meta := paginate(kg.AllNodes(), p)
	edges, _ := paginate(kg.AllEdges(), p)
	meta["nodes"] = nodes
	meta["edges"] = edges
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
