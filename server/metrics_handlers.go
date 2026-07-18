package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/darkcode/capability"
	"github.com/darkcode/config"
	"github.com/darkcode/llm"
	"github.com/darkcode/provider"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/metrics"
	"github.com/darkcode/observability"
)

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": config.Providers(),
		"count":     len(config.Providers()),
	})
}

// handleModelsFetch hits a provider's /models endpoint dynamically to retrieve
// the actual available models for the user's API key.
func (s *Server) handleModelsFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Lookup provider in catalogue to figure out auth scheme.
	p, ok := config.LookupProvider(req.Provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}

	// SECURITY: a caller-supplied base_url combined with an API key is an
	// exfiltration vector (the key is sent in the Authorization header to
	// whatever URL the caller picks). Ignore the caller's base_url whenever a
	// key is involved and always use the catalogue base URL for that provider.
	// A custom base_url is only honoured for local (keyless) providers and for
	// the explicit "openai-compatible" custom provider (CustomBaseURL), where
	// the user is intentionally pointing at their own endpoint.
	apiKey := req.APIKey
	if p.AuthScheme == config.AuthNone {
		apiKey = "" // local providers never need a key
	}
	allowLocal := p.Local || p.CustomBaseURL
	baseURL := p.BaseURL
	if allowLocal && req.BaseURL != "" {
		baseURL = req.BaseURL // allow overriding a local Ollama/LM Studio URL or a custom OpenAI-compatible endpoint
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	// SSRF guard: only http(s) to safe destinations. Local / custom-base
	// providers are allowed to point at loopback/private addresses (a local
	// vLLM or a private proxy); everything else must be a public host.
	if !ssrfGuard(baseURL, allowLocal) {
		writeError(w, http.StatusBadRequest, "blocked: base_url is not an allowed http(s) destination")
		return
	}

	var models []string
	var err error

	if p.ID == "embedded" {
		// Use the shared singleton (embedded.Default) exactly as startup
		// configured it — do NOT call Configure here with a CWD-relative
		// "./models": that unconditionally overwrites the singleton's
		// modelsDir on every call, clobbering the system-wide
		// ~/.darkcode/models path app_wireup.go set at startup (local-first
		// upgrade §6b) with a stale per-directory path the moment a user
		// opens the GUI's model picker for local models.
		embProv := embedded.Default()
		ggufModels, mErr := embProv.ListModels(r.Context())
		if mErr != nil {
			err = mErr
		} else {
			for _, m := range ggufModels {
				models = append(models, m.ID)
			}
		}
	} else {
		// Use the unified backend fetch request
		models, err = provider.FetchModels(p, apiKey, baseURL)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch models dynamically: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
		"count":  len(models),
	})
}

// handleModelsPing is the explicit "test connection" action (local-first
// upgrade §4c): verifies an OpenAI-compatible endpoint is actually reachable
// on demand, rather than only discovering a broken connection when a real
// chat request fails. Shares handleModelsFetch's security posture exactly
// (provider-catalogue lookup, key/base_url sanitization to prevent an
// exfiltration vector, SSRF guard) since it's the same shape of request:
// caller-supplied provider/key/base_url triggering an outbound network call.
func (s *Server) handleModelsPing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	p, ok := config.LookupProvider(req.Provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}

	// See handleModelsFetch's identical block for the security rationale.
	apiKey := req.APIKey
	if p.AuthScheme == config.AuthNone {
		apiKey = ""
	}
	allowLocal := p.Local || p.CustomBaseURL
	baseURL := p.BaseURL
	if allowLocal && req.BaseURL != "" {
		baseURL = req.BaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	if !ssrfGuard(baseURL, allowLocal) {
		writeError(w, http.StatusBadRequest, "blocked: base_url is not an allowed http(s) destination")
		return
	}

	if p.ID == "embedded" {
		// Local models have their own health/readiness machinery (process
		// startup wait-for-healthy) — connectivity in the OpenAI-compatible
		// sense doesn't apply the same way; report status from the shared
		// singleton instead of trying to Ping a URL that may not exist yet.
		st := embedded.Default().Status()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":     st.State == embedded.StateRunning,
			"status": st.State.String(),
		})
		return
	}

	client := llm.NewClient(baseURL, apiKey, req.Model)
	client.SetProvider(p.ID)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleModelsDisable temporarily takes a registered model out of routing/
// consensus selection (local-first upgrade §6c) — the GUI counterpart of
// the CLI's "/models disable". Thin wrapper over Kernel.DisableModel, which
// delegates to the router; the disable is live-only (not persisted to
// config) and expires automatically after duration.
func (s *Server) handleModelsDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model    string `json:"model"`
		Duration string `json:"duration"` // Go duration string, e.g. "1h"; default 1h
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	dur := time.Hour
	if req.Duration != "" {
		d, err := time.ParseDuration(req.Duration)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid duration: "+err.Error())
			return
		}
		dur = d
	}
	if s.kernel == nil {
		writeError(w, http.StatusServiceUnavailable, "kernel not initialized")
		return
	}
	until := time.Now().Add(dur)
	s.kernel.DisableModel(req.Model, until)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "model": req.Model, "disabled_until": until})
}

// handleModelsEnable reverses a temporary disable early — the GUI
// counterpart of "/models enable". A no-op if the model wasn't disabled.
func (s *Server) handleModelsEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if s.kernel == nil {
		writeError(w, http.StatusServiceUnavailable, "kernel not initialized")
		return
	}
	s.kernel.EnableModel(req.Model)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "model": req.Model})
}

// handleMetricsTokens returns the full usage snapshot for the dashboard.
func (s *Server) handleMetricsTokens(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, metrics.Default.Snapshot())
}

// handleMetricsRequests returns recent request records.
func (s *Server) handleMetricsRequests(w http.ResponseWriter, r *http.Request) {
	snap := metrics.Default.Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requests": snap.Recent,
		"count":    len(snap.Recent),
	})
}

// handleMetricsReset clears all accumulated usage metrics.
func (s *Server) handleMetricsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	metrics.Default.Reset()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "metrics reset",
	})
}

// handleCapability returns the hardware metrics and resolved execution tier.
func (s *Server) handleCapability(w http.ResponseWriter, r *http.Request) {
	caps, err := capability.Detect(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to detect capabilities")
		return
	}
	tier := capability.AssignTier(caps)
	hw := observability.GetHardwareStats()
	
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"hardware": hw,
		"tier":     tier.String(),
	})
}

// handleCascade returns the cognition-cascade telemetry: the recent per-query
// rung log (which rung answered and why, with confidence + provenance) and
// per-rung lifetime stats including the current auto-calibrated thresholds.
// This is the local-first upgrade's cost-savings proof surface.
func (s *Server) handleCascade(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil {
		writeError(w, http.StatusServiceUnavailable, "kernel not initialized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"log":   s.kernel.CascadeLog(),
		"stats": s.kernel.CascadeStats(),
	})
}

// fileEntry is a single node in the workspace file listing.
type fileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`     // unix seconds
	ModStr  string `json:"mod_time_str"` // RFC3339
	IsDir   bool   `json:"is_dir"`
	Ext     string `json:"ext"`
}

// handleFilesList returns a recursive listing of the server's current working
// directory so the chat console can render a live workspace browser.
