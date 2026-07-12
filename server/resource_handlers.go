package server

import (
	"net/http"
	"runtime"

	"github.com/darkcode/provider/embedded"
)

func (s *Server) handleSystemResources(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	resp := map[string]interface{}{
		"cpu_percent": 0.0, // OS CPU requires external package, omitting for simplicity
		"mem_used":    m.Alloc,
		"mem_total":   m.Sys,
		"mem_percent": 0.0,
		"goroutines":  runtime.NumGoroutine(),
		"gc_cycles":   m.NumGC,
		"heap_alloc":  m.HeapAlloc,
		"stack_inuse": m.StackInuse,
		// embedded status is piggy-backed onto this 3s poll so the header
		// local-LLM badge stays live without adding a separate poll/endpoint.
		"embedded": s.embeddedStatus(),
	}

	writeJSON(w, http.StatusOK, resp)
}

// embeddedStatus returns a serialisable snapshot of the local/embedded LLM
// provider for the UI. Returns nil when local LLM is disabled (the frontend
// treats a nil/absent field as "not running").
func (s *Server) embeddedStatus() map[string]interface{} {
	s.cfgMu.RLock()
	enabled := s.cfg.EnableLocalLLM
	role := s.cfg.LocalModelRole
	s.cfgMu.RUnlock()
	if !enabled {
		return nil
	}
	emb := embedded.Default()
	st := emb.Status()
	info := map[string]interface{}{
		"is_running":  st.State == embedded.StateRunning,
		"state":       st.State.String(),
		"model_id":    emb.LoadedModelID(),
		"base_url":    st.BaseURL,
		"role":        role,
		"is_primary":  false,
	}
	// Determine is_primary + effective role from the router's runtime state
	// (the local model may have been marked primary or assigned a role via
	// the UI, which is reflected in the router, not just the config).
	if s.kernel != nil {
		for _, m := range s.kernel.RegisteredModels() {
			if m.Name == info["model_id"] && m.Name != "" {
				info["is_primary"] = m.IsPrimary
				if m.Role != "" {
					info["role"] = m.Role
				}
				break
			}
		}
	}
	return info
}
