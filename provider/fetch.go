package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/darkcode/config"
)

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
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return nil, fmt.Errorf("failed to parse Google JSON response: %w", err)
		}
		for _, m := range result.Models {
			name := strings.TrimPrefix(m.Name, "models/")
			models = append(models, name)
		}
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
