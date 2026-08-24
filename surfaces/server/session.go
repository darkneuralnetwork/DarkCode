package server

// session.go — which surface the user is actually looking at.
//
// DarkCode runs one orchestrator behind two front ends, and only one of them
// owns the terminal at a time. This tracks that: the active project, the
// browser connecting and disconnecting over SSE, and the handover back to the
// CLI when the last tab closes.
//
// The disconnect path is deliberately patient. A tab refresh drops the SSE
// connection exactly like a closed browser does, so switching back to the CLI
// on the first disconnect would yank the terminal out from under a reload.

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func (s *Server) setTaskActive(id string, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active {
		s.activeTasks[id] = true
	} else {
		delete(s.activeTasks, id)
	}
}

func (s *Server) getActiveTasks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]string, 0, len(s.activeTasks))
	for id := range s.activeTasks {
		tasks = append(tasks, id)
	}
	return tasks
}

// activeProjectID returns the id of the currently-active project, if any.
// The active project is tracked by the frontend (localStorage) and re-applied
// on each workspace switch so the server can echo it back in /api/workspace.
func (s *Server) activeProjectID() string {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	return s.activeProject
}

// SetActiveProject records which project currently owns the workspace and
// switches the workspace to that project's path. An empty id clears both.
func (s *Server) SetActiveProject(id string) {
	s.wsMu.Lock()
	s.activeProject = id
	s.wsMu.Unlock()
}

// ============================================================================
// FILESYSTEM BROWSER — for directory picker in project creation
// ============================================================================

// handleFSBrowse lists directories at a given absolute path for the directory
// picker UI. Unlike workspace/browse, this is unrestricted to any workspace and
// only returns directories (not files). Query: ?path=<abs_path>.
func (s *Server) handleSwitchCLI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Non-blocking send to SwitchToCLI
	s.signalSwitchToCLI(req.ProjectID)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "switching"})
}

// signalSwitchToCLI performs a non-blocking send on SwitchToCLI. If main.go
// is not currently blocked on the receive (e.g. not in GUI mode), the signal
// is dropped — this is intentional and avoids blocking the HTTP/SSE path.
func (s *Server) signalSwitchToCLI(projectID string) {
	select {
	case s.SwitchToCLI <- projectID:
	default:
	}
}

// SetGUIActive arms (GUI) or disarms (CLI) disconnect-driven CLI resume.
// main.go calls it on every CLI↔GUI transition. Disarming in CLI mode stops a
// leftover browser tab's SSE disconnect from firing the grace timer and
// corrupting the terminal prompt.
func (s *Server) SetGUIActive(active bool) {
	s.guiMu.Lock()
	defer s.guiMu.Unlock()
	if active {
		if s.guiGrace != nil {
			s.guiGrace.Stop()
			s.guiGrace = nil
		}
		s.sseEverConnected = false
		s.ResumeOnDisconnect = true
	} else {
		if s.guiGrace != nil {
			s.guiGrace.Stop()
			s.guiGrace = nil
		}
		s.sseEverConnected = false
		s.ResumeOnDisconnect = false
	}
}

// BeginGUISession is retained for compatibility; it is equivalent to
// SetGUIActive(true). New callers should use SetGUIActive.
func (s *Server) BeginGUISession() { s.SetGUIActive(true) }

// onSSEConnect is called when an SSE client connects. It cancels any pending
// disconnect grace timer (e.g. the browser refreshed and reconnected within
// the grace window) and records that the GUI has been used this session.
func (s *Server) onSSEConnect() {
	s.guiMu.Lock()
	defer s.guiMu.Unlock()
	if s.guiGrace != nil {
		s.guiGrace.Stop()
		s.guiGrace = nil
	}
	s.sseEverConnected = true
}

// onSSEDisconnect is called when an SSE client disconnects (browser closed,
// tab navigated away, network drop). If this was the last subscriber and the
// GUI has been used this session, it arms a grace timer; when the timer fires
// (no new client reconnected within the window) it signals main.go to resume
// CLI mode.
func (s *Server) onSSEDisconnect() {
	s.guiMu.Lock()
	if !s.ResumeOnDisconnect || !s.sseEverConnected {
		s.guiMu.Unlock()
		return
	}
	// Re-check the subscriber count under guiMu; if a new client already
	// reconnected there is nothing to do.
	if s.emitter.SubscriberCount() > 0 {
		s.guiMu.Unlock()
		return
	}
	log.Printf("[gui] last SSE client gone; arming %v resume-CLI grace", guiDisconnectGrace)
	if s.guiGrace != nil {
		s.guiGrace.Stop()
	}
	s.guiGrace = time.AfterFunc(guiDisconnectGrace, func() {
		s.guiMu.Lock()
		// Re-check: a client may have reconnected during the grace window.
		if s.emitter.SubscriberCount() > 0 {
			s.guiGrace = nil
			s.guiMu.Unlock()
			return
		}
		s.sseEverConnected = false
		s.guiGrace = nil
		pid := s.activeProjectID()
		log.Printf("[gui] grace fired; resuming CLI project=%q subs=%d", pid, s.emitter.SubscriberCount())
		s.guiMu.Unlock()
		s.signalSwitchToCLI(pid)
	})
	s.guiMu.Unlock()
}

func (s *Server) handleSessionState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_project": s.activeProjectID(),
	})
}
