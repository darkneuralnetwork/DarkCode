package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if s.projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project store not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := s.projects.List()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"projects": list,
			"count":    len(list),
		})
	case http.MethodPost:
		var req struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Path        string   `json:"path"`
			Tags        []string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		p, err := s.projects.Create(req.Name, req.Description, req.Path, req.Tags)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Seed plan + workflow immediately so the Plan & Workflow tab is never
		// empty for a new project (skeleton now, LLM rewrite async).
		s.seedProjectPlanWorkflow(p.ID, p.Name, p.Description, "")
		writeJSON(w, http.StatusCreated, p)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleProjectItem handles a single project: /api/projects/{id}, plus the
// /api/projects/{id}/context sub-path for reading/writing the context body.
func (s *Server) handleProjectItem(w http.ResponseWriter, r *http.Request) {
	if s.projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project store not initialized")
		return
	}
	// Path is like /api/projects/<id> or /api/projects/<id>/context
	rest := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusBadRequest, "project id is required")
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = strings.TrimSuffix(parts[1], "/")
	}

	// Sub-resource: context.md body.
	if sub == "context" {
		switch r.Method {
		case http.MethodGet:
			ctx, err := s.projects.GetContext(id)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "context": ctx})
		case http.MethodPut, http.MethodPost:
			var req struct {
				Context string `json:"context"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := s.projects.SetContext(id, req.Context); err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			// Trigger automatic rewrite on manual edit so the user's edits are
			// kept compact, just like chat turns.
			go s.maybeRewriteProjectContext(id)
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "ok": true})
		default:
			writeError(w, http.StatusMethodNotAllowed, "use GET or PUT")
		}
		return
	}

	// Sub-resource: manual context rewrite via UI button
	if sub == "context/rewrite" && r.Method == http.MethodPost {
		// Run it synchronously so the UI can await it and reload.
		s.maybeRewriteProjectContext(id)
		writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "ok": true})
		return
	}

	// Sub-resource: plan body.
	if sub == "plan" {
		switch r.Method {
		case http.MethodGet:
			plan, err := s.projects.GetPlan(id)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "plan": plan})
		case http.MethodPut, http.MethodPost:
			var req struct {
				Plan string `json:"plan"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := s.projects.SetPlan(id, req.Plan); err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "ok": true})
		default:
			writeError(w, http.StatusMethodNotAllowed, "use GET or PUT")
		}
		return
	}

	// Sub-resource: workflow body.
	if sub == "workflow" {
		switch r.Method {
		case http.MethodGet:
			workflow, err := s.projects.GetWorkflow(id)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "workflow": workflow})
		case http.MethodPut, http.MethodPost:
			var req struct {
				Workflow string `json:"workflow"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := s.projects.SetWorkflow(id, req.Workflow); err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "ok": true})
		default:
			writeError(w, http.StatusMethodNotAllowed, "use GET or PUT")
		}
		return
	}

	// Sub-resource: regenerate plan/workflow via the LLM rewriter (Blueprint
	// task board "Regenerate" button). kind is "plan" or "workflow".
	if sub == "plan/regenerate" && r.Method == http.MethodPost {
		s.regeneratePlanWorkflow(id, "plan")
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"id": id, "ok": true, "kind": "plan"})
		return
	}
	if sub == "workflow/regenerate" && r.Method == http.MethodPost {
		s.regeneratePlanWorkflow(id, "workflow")
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"id": id, "ok": true, "kind": "workflow"})
		return
	}

	// Sub-resource: open (touch LastOpened + switch the chat workspace to
	// this project's path so the file explorer follows the project).
	if sub == "open" && r.Method == http.MethodPost {
		if err := s.projects.Touch(id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		p, _ := s.projects.GetWithContext(id)
		// Seed plan + workflow for a project opened for the first time (or whose
		// plan/workflow files are missing) so the tab is never empty.
		if p != nil {
			s.seedProjectPlanWorkflow(p.ID, p.Name, p.Description, p.Context)
		}
		// If the project declares a working directory, make it the active
		// workspace so the chat console's file explorer switches too. A
		// missing/invalid path is ignored (the workspace stays as-is) so
		// opening a project without a path doesn't error.
		if p != nil && strings.TrimSpace(p.Path) != "" {
			if err := s.setActiveWorkspace(p.Path); err != nil {
				// Don't fail the open call — the project is still activated;\n				// just report that the workspace didn't switch.
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"id":          id,
					"name":        p.Name,
					"path":        p.Path,
					"description": p.Description,
					"tags":        p.Tags,
					"created_at":  p.CreatedAt,
					"updated_at":  p.UpdatedAt,
					"last_opened": p.LastOpened,
					"context":     p.Context,
					"workspace":   s.ActiveWorkspace(),
					"warning":     "project opened but workspace not switched: " + err.Error(),
				})
				return
			}
			s.SetActiveProject(id)
		} else {
			s.SetActiveProject(id)
		}
		writeJSON(w, http.StatusOK, p)
		return
	}

	// The project resource itself.
	switch r.Method {
	case http.MethodGet:
		p, err := s.projects.GetWithContext(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodPut, http.MethodPatch:
		var req struct {
			Name        *string  `json:"name"`
			Description *string  `json:"description"`
			Path        *string  `json:"path"`
			Tags        []string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		p, err := s.projects.Update(id, req.Name, req.Description, req.Path, req.Tags)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodDelete:
		if err := s.projects.Delete(id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET, PUT, or DELETE")
	}
}

// handleMemory returns memory system overview.
