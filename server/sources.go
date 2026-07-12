package server

// ============================================================================
// TOOL SOURCES — REST API for connecting/disconnecting tool sources at runtime.
//
// A tool source contributes tools to the registry from outside the built-in
// set: an MCP server (stdio or HTTP) or an in-house Internal Tool Format
// file. These endpoints let the GUI (and any REST client) add, connect,
// disconnect, and remove sources while the agent is running.
//
// Routes (registered in server.go):
//
//   GET    /api/tools/sources            list all sources + status
//   POST   /api/tools/sources            add a new source (and optionally connect)
//   GET    /api/tools/sources/{id}       get one source
//   DELETE /api/tools/sources/{id}       disconnect + remove a source
//   POST   /api/tools/sources/{id}/connect    connect a disconnected source
//   POST   /api/tools/sources/{id}/disconnect disconnect a connected source
// ============================================================================

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/darkcode/config"
	"github.com/darkcode/tools"
)

// sourcesRequest is the body for POST /api/tools/sources.
type sourcesRequest struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Path        string            `json:"path,omitempty"`
	AutoConnect bool              `json:"auto_connect,omitempty"`
	Connect     bool              `json:"connect,omitempty"` // connect immediately after adding
}

// handleToolSources dispatches the collection routes.
func (s *Server) handleToolSources(w http.ResponseWriter, r *http.Request) {
	// Sub-paths like /api/tools/sources/{id}/connect are handled elsewhere.
	if s.sources == nil {
		writeError(w, http.StatusServiceUnavailable, "tool source manager not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listToolSources(w, r)
	case http.MethodPost:
		s.addToolSource(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleToolSourceItem dispatches the single-resource + action routes.
func (s *Server) handleToolSourceItem(w http.ResponseWriter, r *http.Request) {
	if s.sources == nil {
		writeError(w, http.StatusServiceUnavailable, "tool source manager not initialized")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tools/sources/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusBadRequest, "source id is required")
		return
	}
	action := ""
	if len(parts) == 2 {
		action = strings.TrimSuffix(parts[1], "/")
	}

	switch action {
	case "":
		// /api/tools/sources/{id}
		switch r.Method {
		case http.MethodGet:
			s.getToolSource(w, r, id)
		case http.MethodDelete:
			s.removeToolSource(w, r, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, "use GET or DELETE")
		}
	case "connect":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		s.connectToolSource(w, r, id)
	case "disconnect":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		s.disconnectToolSource(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "unknown action: "+action)
	}
}

func (s *Server) listToolSources(w http.ResponseWriter, r *http.Request) {
	srcs := s.sources.List()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sources": srcs,
		"count":   len(srcs),
	})
}

func (s *Server) getToolSource(w http.ResponseWriter, r *http.Request, id string) {
	src, ok := s.sources.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "source not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, src)
}

func (s *Server) addToolSource(w http.ResponseWriter, r *http.Request) {
	var req sourcesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	cfg := tools.SourceConfig{
		ID:          req.ID,
		Name:        req.Name,
		Type:        tools.SourceType(req.Type),
		Command:     req.Command,
		Args:        req.Args,
		Env:         req.Env,
		URL:         req.URL,
		Headers:     req.Headers,
		Path:        req.Path,
		AutoConnect: req.AutoConnect,
	}
	id, err := s.sources.Add(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persist to .config so the source survives restarts.
	s.persistSources()

	if req.Connect || req.AutoConnect {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := s.sources.Connect(ctx, id); err != nil {
			// The source was added but failed to connect — report a partial
			// success with the error so the UI can show the failure.
			src, _ := s.sources.Get(id)
			writeJSON(w, http.StatusCreated, map[string]interface{}{
				"id":      id,
				"source":  src,
				"warning": "source added but failed to connect: " + err.Error(),
			})
			return
		}
	}
	src, _ := s.sources.Get(id)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":     id,
		"source": src,
	})
}

func (s *Server) removeToolSource(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.sources.Remove(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.persistSources()
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "ok": true})
}

func (s *Server) connectToolSource(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := s.sources.Connect(ctx, id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	src, _ := s.sources.Get(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "source": src, "ok": true})
}

func (s *Server) disconnectToolSource(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.sources.Disconnect(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	src, _ := s.sources.Get(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "source": src, "ok": true})
}

// persistSources writes the current set of source definitions back into the
// in-memory config and saves .config to disk. It is best-effort: a failure to
// save does not roll back the in-memory change.
func (s *Server) persistSources() {
	if s.cfg == nil {
		return
	}
	cfgs := s.sources.Configs()
	out := make([]config.ToolSourceConfig, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, config.ToolSourceConfig{
			ID:          c.ID,
			Name:        c.Name,
			Type:        string(c.Type),
			Command:     c.Command,
			Args:        c.Args,
			Env:         c.Env,
			URL:         c.URL,
			Headers:     c.Headers,
			Path:        c.Path,
			AutoConnect: c.AutoConnect,
		})
	}
	s.cfgMu.Lock()
	s.cfg.ToolSources = out
	_ = s.cfg.Save()
	s.cfgMu.Unlock()
}
