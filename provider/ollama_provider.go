package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/llm"
)

type OllamaProvider struct {
	id      string
	name    string
	baseURL string
}

func NewOllamaProvider(baseURL string) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaProvider{
		id:      "ollama",
		name:    "Ollama",
		baseURL: baseURL,
	}
}

func (p *OllamaProvider) ID() string {
	return p.id
}

func (p *OllamaProvider) Name() string {
	return p.name
}

func (p *OllamaProvider) Type() ProviderType {
	return ProviderLocal
}

func (p *OllamaProvider) IsAvailable(ctx context.Context) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", p.baseURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *OllamaProvider) ListModels(ctx context.Context) ([]core.ModelMetadata, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", p.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var models []core.ModelMetadata
	for _, m := range result.Models {
		models = append(models, core.ModelMetadata{
			ID:      m.Name,
			Context: 8192,
		})
	}
	return models, nil
}

func (p *OllamaProvider) CreateClient(modelID string, opts ClientOptions) (core.LLMClient, error) {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = p.baseURL + "/v1" // Ollama's OpenAI-compatible endpoint
	}
	c := llm.NewClient(baseURL, "ollama", modelID)
	c.SetProvider(p.id)
	return c, nil
}

func (p *OllamaProvider) Close() error {
	return nil
}
