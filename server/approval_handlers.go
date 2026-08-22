package server

import (
	"encoding/json"
	"net/http"

	"github.com/darkcode/permission"
)

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if s.approver == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"approvals": []interface{}{}, "count": 0})
		return
	}
	pending := s.approver.Pending()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"approvals": pending,
		"count":     len(pending),
	})
}

// handleApprovalDecide resolves a pending approval request. The web UI calls
// this when the user clicks Allow Once / Allow Session / Deny in the popup.
// The body may carry an optional "feedback" string — a free-form steer the
// user typed (e.g. "use /tmp instead of /var") which is surfaced back to the
// agent through the tool-result channel so it adapts mid-execution.
//
// Body: {"id":"appr-1","decision":"allow-once","feedback":"…"}
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if s.approver == nil {
		writeError(w, http.StatusServiceUnavailable, "permission system not active")
		return
	}
	var req struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
		Feedback string `json:"feedback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ID == "" || req.Decision == "" {
		writeError(w, http.StatusBadRequest, "id and decision are required")
		return
	}
	decision := permission.ParseDecision(req.Decision)
	if s.approver.Resolve(req.ID, decision, req.Feedback) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":       true,
			"id":       req.ID,
			"decision": decision.String(),
		})
	} else {
		writeError(w, http.StatusNotFound, "approval not found (already resolved or expired)")
	}
}

// ============================================================================
// PROJECTS — long-lived per-project context (file-backed)
// ============================================================================

// handleProjects handles the collection: GET = list, POST = create.
