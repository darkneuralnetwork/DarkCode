package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/darkcode/capability"
	"github.com/darkcode/config"
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
		// Ensure ./models directory exists to prevent fatal errors
		_ = os.MkdirAll("./models", 0755)
		// Use the shared singleton (embedded.Default) so the model list is
		// consistent with the running server. Configure sets the models dir;
		// empty binaryDir is ignored so it can't clobber the startup dir.
		embProv := embedded.Configure("./models", "")
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
