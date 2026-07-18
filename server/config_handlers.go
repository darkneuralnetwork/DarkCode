package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/darkcode/config"
	"github.com/darkcode/metrics"
	"github.com/darkcode/orchestrator"
)

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		s.updateConfig(w, r)
		return
	}

	// Snapshot EVERY field under the read lock. updateConfig (POST /api/config)
	// takes the write lock for its whole body and mutates these fields
	// (RoutingMode/SafetyLevel/MaxTurns/AgenticLoop/MaxLoops/CompressorModel/…).
	// Reading them here without the lock raced a concurrent settings save —
	// the same class of bug previously fixed in handleStatus. One contiguous
	// critical section; the masked-models loop stays where it is.
	s.cfgMu.RLock()
	safeModels := make(map[string]config.ModelConfig)
	for k, v := range s.cfg.Models {
		v.APIKey = maskKey(v.APIKey)
		safeModels[k] = v
	}
	model := s.cfg.Model
	provider := s.cfg.Provider
	baseURL := s.cfg.BaseURL
	hasKey := s.cfg.APIKey != ""
	routingMode := s.cfg.RoutingMode
	safetyLevel := s.cfg.SafetyLevel
	maxTurns := s.cfg.MaxTurns
	compressContext := s.cfg.CompressContext
	agenticLoop := s.cfg.AgenticLoop
	maxLoops := s.cfg.MaxLoops
	compressorModel := s.cfg.CompressorModel
	uiMode := s.cfg.UIMode
	contextLength := s.cfg.ContextLength
	maxConcurrent := s.cfg.MaxConcurrent
	temperature := s.cfg.Temperature
	executionProfile := s.cfg.ExecutionProfile
	s.cfgMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":            model,
		"provider":         provider,
		"base_url":         baseURL,
		"routing_mode":     routingMode,
		"safety_level":     safetyLevel,
		"max_turns":        maxTurns,
		"compress_context": compressContext,
		"agentic_loop":     agenticLoop,
		"max_loops":        maxLoops,
		"compressor_model": compressorModel,
		"ui_mode":          uiMode,
		"context_length":   contextLength,
		"max_concurrent":   maxConcurrent,
		"temperature":      temperature,
		"execution_profile": executionProfile,
		"enable_local_llm": s.cfg.EnableLocalLLM,
		"local_mode":       s.cfg.ResolvedLocalMode(),
		"force_local":      s.cfg.ForceLocal(),
		"local_model_role": s.cfg.LocalModelRole,
		"memory_profile":   s.cfg.MemoryProfile,
		"has_api_key":      hasKey,
		"models":           safeModels,
		"embedded":         s.embeddedStatus(),
		"registered_models": s.kernelRegisteredModels(),
		"metrics":          metrics.Default.Snapshot(),
	})
}

func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	var req struct {
		Action      string `json:"action"` // update_settings | add_model | remove_model | set_primary
		RoutingMode string `json:"routing_mode,omitempty"`
		SafetyLevel string `json:"safety_level,omitempty"`
		MaxTurns    int    `json:"max_turns,omitempty"`

		// Agentic loop (looping technology) toggle.
		AgenticLoop *bool `json:"agentic_loop,omitempty"`
		MaxLoops    int   `json:"max_loops,omitempty"`

		// Execution profile (parallelism switcher): "parallel" | "sequential" |
		// "auto". Pointer so an explicit "auto" is distinguishable from "not sent".
		ExecutionProfile *string `json:"execution_profile,omitempty"`

		// MemoryProfile is the local model's context/RAM knob: "lean" | "balanced"
		// | "max" | "" (auto). Pointer so an unset field leaves it unchanged.
		MemoryProfile *string `json:"memory_profile,omitempty"`

		EnableLocalLLM *bool `json:"enable_local_llm,omitempty"`
		// LocalMode sets the three-plus-state local preference directly:
		// "off" | "auto" | "on" | "force". "force" pins routing to the local
		// model (no cloud fallback) and starts it on demand. Pointer so an
		// unset field leaves the current mode unchanged.
		LocalMode *string `json:"local_mode,omitempty"`

		ModelName       string `json:"model_name,omitempty"`
		Provider        string `json:"provider,omitempty"`
		APIKey          string `json:"api_key,omitempty"`
		BaseURL         string `json:"base_url,omitempty"`
		Role            string `json:"role,omitempty"`             // consensus role (set_role action)
		CompressorModel string `json:"compressor_model,omitempty"` // model for context compression (set_compressor)
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	switch req.Action {
	case "add_model":
		if req.ModelName == "" {
			writeError(w, http.StatusBadRequest, "model_name is required")
			return
		}
		if s.cfg.Models == nil {
			s.cfg.Models = make(map[string]config.ModelConfig)
		}

		providerID := req.Provider
		if providerID == "" {
			providerID = "openrouter" // sensible default
		}

		// Resolve base URL + auth scheme from the provider registry.
		baseURL := req.BaseURL
		if baseURL == "" {
			if p, ok := config.LookupProvider(providerID); ok {
				baseURL = p.BaseURL
			} else {
				writeError(w, http.StatusBadRequest, "unknown provider: "+providerID)
				return
			}
		}

		apiKey := req.APIKey
		// For local providers, no key is needed; use a placeholder.
		if p, ok := config.LookupProvider(providerID); ok && p.Local && apiKey == "" {
			apiKey = "local"
		}

		s.cfg.Models[req.ModelName] = config.ModelConfig{
			Model:    req.ModelName,
			Provider: providerID,
			APIKey:   apiKey,
			BaseURL:  baseURL,
			Tier:     config.ResolveTier(providerID, req.ModelName),
		}

		// Set the newly registered model as the primary active model ONLY when
		// no primary is currently configured. Previously this unconditionally
		// stole primary on every add_model — so adding a second model for
		// consensus/verification would silently swap the user's active chat
		// model out from under them. Now we preserve an existing primary.
		if s.cfg.Model == "" {
			s.cfg.Model = req.ModelName
			s.cfg.BaseURL = baseURL
			s.cfg.APIKey = apiKey
			s.cfg.Provider = providerID
		}

	case "remove_model":
		if req.ModelName == "" {
			writeError(w, http.StatusBadRequest, "model_name is required")
			return
		}
		// The embedded/local model is a runtime entity, not a persisted
		// config entry — it can't be "removed". The user toggles
		// enable_local_llm instead.
		if strings.HasPrefix(req.ModelName, "embedded/") {
			writeError(w, http.StatusBadRequest, "local model cannot be removed; toggle 'Local LLM' in settings instead")
			return
		}
		delete(s.cfg.Models, req.ModelName)
		if s.cfg.Model == req.ModelName {
			// Fall back to another registered model if present.
			fallbackFound := false
			for _, mc := range s.cfg.Models {
				s.cfg.Model = mc.Model
				s.cfg.BaseURL = mc.BaseURL
				s.cfg.APIKey = mc.APIKey
				s.cfg.Provider = mc.Provider
				fallbackFound = true
				break
			}
			if !fallbackFound {
				s.cfg.Model = ""
				s.cfg.BaseURL = ""
				s.cfg.APIKey = ""
				s.cfg.Provider = ""
			}
		}

	case "set_primary":
		// Embedded/local model: not in cfg.Models (it's a runtime entity).
		// Clearing the cloud primary fields makes the embedded model the
		// de-facto primary (ReloadModels marks it primary when cfg.Model == "").
		if strings.HasPrefix(req.ModelName, "embedded/") {
			s.cfg.Model = ""
			s.cfg.BaseURL = ""
			s.cfg.APIKey = ""
			s.cfg.Provider = ""
			break
		}
		if mc, ok := s.cfg.Models[req.ModelName]; ok {
			s.cfg.Model = mc.Model
			s.cfg.BaseURL = mc.BaseURL
			s.cfg.APIKey = mc.APIKey
			s.cfg.Provider = mc.Provider
		} else {
			writeError(w, http.StatusBadRequest, "model not registered: "+req.ModelName)
			return
		}

	case "set_role":
		// Set the consensus role for a registered model (critic, skeptic,
		// knowledge_booster, …). The primary model's role is fixed (synthesizer)
		// and cannot be changed here.
		if req.ModelName == "" {
			writeError(w, http.StatusBadRequest, "model_name is required")
			return
		}
		if req.ModelName == s.cfg.Model {
			writeError(w, http.StatusBadRequest, "primary model is always the synthesizer")
			return
		}
		// Embedded/local model: persist the role in LocalModelRole and apply
		// immediately at runtime via the kernel (the model is not in
		// cfg.Models, so the normal config-map path doesn't apply).
		if strings.HasPrefix(req.ModelName, "embedded/") {
			// When cfg.Model is empty the embedded model IS the primary —
			// reject the role change for consistency with cloud primaries.
			if s.cfg.Model == "" {
				writeError(w, http.StatusBadRequest, "primary model is always the synthesizer")
				return
			}
			s.cfg.LocalModelRole = req.Role
			if s.kernel != nil {
				s.kernel.SetModelRole(req.ModelName, req.Role)
			}
			break
		}
		mc, ok := s.cfg.Models[req.ModelName]
		if !ok {
			writeError(w, http.StatusBadRequest, "model not registered: "+req.ModelName)
			return
		}
		mc.Role = req.Role
		s.cfg.Models[req.ModelName] = mc

	case "set_compressor":
		// Set the model used for context compression (Layer 3). Any registered
		// model can be the compressor. An empty model_name resets to the default
		// (primary model).
		if req.CompressorModel != "" {
			if _, ok := s.cfg.Models[req.CompressorModel]; !ok {
				writeError(w, http.StatusBadRequest, "model not registered: "+req.CompressorModel)
				return
			}
		}
		s.cfg.CompressorModel = req.CompressorModel

	case "update_settings":
		if req.RoutingMode != "" {
			s.cfg.RoutingMode = req.RoutingMode
		}
		if req.SafetyLevel != "" {
			s.cfg.SafetyLevel = req.SafetyLevel
		}
		if req.MaxTurns > 0 {
			s.cfg.MaxTurns = req.MaxTurns
		}
		if req.EnableLocalLLM != nil {
			s.cfg.EnableLocalLLM = *req.EnableLocalLLM
		}
		// Memory profile (local model context/RAM knob). Validated against the
		// known names; empty means auto. Takes effect on the next local-model
		// (re)load.
		if req.MemoryProfile != nil {
			switch strings.ToLower(strings.TrimSpace(*req.MemoryProfile)) {
			case "", "lean", "balanced", "max":
				s.cfg.MemoryProfile = strings.ToLower(strings.TrimSpace(*req.MemoryProfile))
			default:
				writeError(w, http.StatusBadRequest, "invalid memory_profile (want lean|balanced|max)")
				return
			}
		}
		// Local mode ("off"|"auto"|"on"|"force"). Applied via
		// ApplyLocalPreference below so force-local pins routing and starts the
		// embedded model without a restart. Keep the legacy EnableLocalLLM bool
		// in sync so a downgrade to an older build still behaves sensibly.
		if req.LocalMode != nil {
			switch *req.LocalMode {
			case "off":
				s.cfg.LocalMode = "off"
				s.cfg.EnableLocalLLM = false
			case "auto", "on", "force":
				s.cfg.LocalMode = *req.LocalMode
				s.cfg.EnableLocalLLM = true
			default:
				writeError(w, http.StatusBadRequest, "invalid local_mode (want off|auto|on|force)")
				return
			}
		}
		// Agentic loop hot-toggle. Pointer so we can distinguish "not sent"
		// from "explicitly false".
		if req.AgenticLoop != nil {
			s.cfg.AgenticLoop = *req.AgenticLoop
		}
		if req.MaxLoops > 0 {
			s.cfg.MaxLoops = req.MaxLoops
		}
		if s.kernel != nil {
			s.kernel.SetAgenticLoop(s.cfg.AgenticLoop, s.cfg.MaxLoops)
		}
		// Execution profile (parallelism switcher) hot-toggle. Applied to the
		// executor + router on the next Execute.
		if req.ExecutionProfile != nil {
			s.cfg.ExecutionProfile = *req.ExecutionProfile
			if s.kernel != nil {
				s.kernel.SetExecutionProfile(*req.ExecutionProfile)
			}
		}

	default:
		writeError(w, http.StatusBadRequest, "invalid action")
		return
	}

	if err := s.cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save config: "+err.Error())
		return
	}

	// Hot-reload model clients so newly added/switched models take effect now.
	if s.kernel != nil {
		s.kernel.ReloadModels(s.cfg)
	}

	// Apply the local-LLM preference last: pin/unpin force-local routing and,
	// when force-local is requested but not yet up, start the embedded model.
	// A force-local failure is reported (never a silent cloud fallback) but
	// does not fail the whole settings save — the rest of the update stuck.
	warning := ""
	if s.kernel != nil {
		if err := s.kernel.ApplyLocalPreference(r.Context(), s.cfg); err != nil {
			warning = err.Error()
		}
	}

	resp := map[string]interface{}{
		"success":     true,
		"message":     "Configuration updated successfully",
		"local_mode":  s.cfg.ResolvedLocalMode(),
		"force_local": s.cfg.ForceLocal(),
	}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// activeTask helpers

// kernelRegisteredModels returns the router's live model list (including the
// runtime embedded model) for the /api/config response. This is separate from
// the persisted cfg.Models map: the embedded model is loaded at runtime and
// never appears in cfg.Models, so without this the UI's "Registered Models"
// list and header switcher can't see it.
func (s *Server) kernelRegisteredModels() []orchestrator.RouterModelInfo {
	if s.kernel == nil {
		return nil
	}
	return s.kernel.RegisteredModels()
}
