package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/darkcode/config"
)

// sortGeminiModels orders models newest-generation-first (e.g. 2.5 before 2.0),
// then alphabetically within a generation, so the picker leads with the most
// current models instead of the provider's arbitrary API order.
func sortGeminiModels(models []string) {
	sort.SliceStable(models, func(i, j int) bool {
		gi := geminiGeneration(strings.ToLower(models[i]))
		gj := geminiGeneration(strings.ToLower(models[j]))
		if gi != gj {
			return gi > gj // higher generation first
		}
		return models[i] < models[j]
	})
}

// geminiVersionRe pulls the generation number out of a Gemini model id, e.g.
// "gemini-2.5-flash" → "2.5", "gemini-1.5-pro-002" → "1.5". Legacy ids without
// a numeric generation ("gemini-pro", "gemini-pro-vision") don't match.
var geminiVersionRe = regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)`)

// geminiSpecialized are substrings marking non-chat or vision-only Google
// models that should never appear in a chat model picker, even if the API
// reports generateContent support for them.
var geminiSpecialized = []string{"embedding", "embed", "aqa", "imagen", "-tts", "vision"}

// geminiGeneration returns the generation number encoded in a Gemini model id
// (e.g. 2.5), or 0 when the id carries no numeric generation (legacy 1.0-era
// names like "gemini-pro", or experimental ids like "gemini-exp-1206").
func geminiGeneration(lowerID string) float64 {
	m := geminiVersionRe.FindStringSubmatch(lowerID)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// keepGeminiModel reports whether a Google model id should be surfaced in the
// picker. It keeps only current, chat-capable models:
//   - must support generateContent (drops embeddings, aqa, and other non-chat
//     models) — but only enforced when the API actually reports methods, so a
//     future response that omits the field doesn't wipe the whole list;
//   - drops specialized/vision families by id;
//   - for the Gemini family, requires generation ≥ 2.0, dropping the
//     deprecated 1.0/1.5 lines and legacy unversioned names.
//
// Non-Gemini chat models the endpoint may return (e.g. learnlm) pass through
// as long as they are chat-capable and not specialized.
func keepGeminiModel(id string, methods []string) bool {
	if len(methods) > 0 && !containsFold(methods, "generateContent") {
		return false
	}
	low := strings.ToLower(id)
	for _, bad := range geminiSpecialized {
		if strings.Contains(low, bad) {
			return false
		}
	}
	if strings.HasPrefix(low, "gemini") {
		return geminiGeneration(low) >= 2.0
	}
	return true
}

// containsFold reports whether list contains target (case-insensitively).
func containsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

// FetchModels dynamically fetches available models from an OpenAI-compatible provider.
func FetchModels(p config.Provider, apiKey string, baseURL string) ([]string, error) {
	if baseURL == "" {
		baseURL = p.BaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	var reqURL string
	
	// Handle provider specific URLs
	if p.ID == "google" {
		// Gemini API compatibility uses /openai, but models fetch uses native API
		baseURL = strings.TrimSuffix(baseURL, "/openai")
		reqURL = baseURL + "/models"
		if apiKey != "" {
			reqURL += "?key=" + apiKey
		}
	} else {
		reqURL = baseURL + "/models"
	}

	httpReq, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Provider-specific auth. Anthropic authenticates exclusively via
	// `x-api-key` + `anthropic-version`; sending `Authorization: Bearer` as
	// well is a dual-auth conflict that Anthropic's /v1/models endpoint
	// rejects (root cause of the Anthropic model-fetch failures). So we
	// short-circuit Anthropic before the generic Bearer/api-key switch.
	if p.ID == "anthropic" && apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else if p.ID == "google" && apiKey != "" {
		// Google's dynamic model fetch uses the `?key=` parameter.
		// Sending an `Authorization: Bearer` header alongside it causes Google to 
		// expect an OAuth token, rejecting standard API keys with HTTP 401.
	} else if apiKey != "" {
		switch p.AuthScheme {
		case config.AuthBearer:
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		case config.AuthAPIKey:
			httpReq.Header.Set("api-key", apiKey)
		}
	}

	for k, v := range p.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider error HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var models []string

	// Parse based on provider
	if p.ID == "google" {
		var result struct {
			Models []struct {
				Name                       string   `json:"name"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return nil, fmt.Errorf("failed to parse Google JSON response: %w", err)
		}
		// Google's /models endpoint returns its entire historical catalog:
		// deprecated 1.0/1.5 generations, non-chat models (embeddings, aqa,
		// imagen, tts), and vision-only variants. Filter to just the usable,
		// current chat models so the picker isn't flooded with obsolete IDs.
		for _, m := range result.Models {
			name := strings.TrimPrefix(m.Name, "models/")
			if keepGeminiModel(name, m.SupportedGenerationMethods) {
				models = append(models, name)
			}
		}
		sortGeminiModels(models)
	} else if p.ID == "anthropic" {
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return nil, fmt.Errorf("failed to parse Anthropic JSON response: %w", err)
		}
		for _, m := range result.Data {
			models = append(models, m.ID)
		}
	} else {
		// Standard OpenAI format: {"data": [{"id": "gpt-4"}, ...]}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return nil, fmt.Errorf("failed to parse JSON response: %w", err)
		}
		for _, m := range result.Data {
			models = append(models, m.ID)
		}
	}

	return models, nil
}
