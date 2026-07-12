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

type LMStudioProvider struct {
	id      string
	name    string
	baseURL string
}

func NewLMStudioProvider(baseURL string) *LMStudioProvider {
	if baseURL == "" {
		baseURL = "http://localhost:1234/v1"
	}
	return &LMStudioProvider{
		id:      "lmstudio",
		name:    "LM Studio",
		baseURL: baseURL,
	}
}

func (p *LMStudioProvider) ID() string {
	return p.id
}

func (p *LMStudioProvider) Name() string {
	return p.name
}

func (p *LMStudioProvider) Type() ProviderType {
	return ProviderLocal
}

func (p *LMStudioProvider) IsAvailable(ctx context.Context) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", p.baseURL+"/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *LMStudioProvider) ListModels(ctx context.Context) ([]core.ModelMetadata, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", p.baseURL+"/models", nil)
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
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var models []core.ModelMetadata
	for _, m := range result.Data {
		models = append(models, core.ModelMetadata{
			ID:      m.ID,
			Context: 16384,
		})
	}
	return models, nil
}

func (p *LMStudioProvider) CreateClient(modelID string, opts ClientOptions) (core.LLMClient, error) {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = p.baseURL
	}
	c := llm.NewClient(baseURL, "lmstudio", modelID)
	c.SetProvider(p.id)
	return c, nil
}

func (p *LMStudioProvider) Close() error {
	return nil
}
