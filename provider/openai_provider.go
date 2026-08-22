package provider

import (
	"context"

	"github.com/darkcode/core"
	"github.com/darkcode/llm"
)

// OpenAIProvider wraps the existing llm.Client for OpenAI-compatible APIs.
type OpenAIProvider struct {
	id   string
	name string
}

func NewOpenAIProvider() *OpenAIProvider {
	return &OpenAIProvider{
		id:   "openai",
		name: "OpenAI Compatible",
	}
}

func (p *OpenAIProvider) ID() string {
	return p.id
}

func (p *OpenAIProvider) Name() string {
	return p.name
}

func (p *OpenAIProvider) Type() ProviderType {
	return ProviderCloud
}

func (p *OpenAIProvider) IsAvailable(ctx context.Context) bool {
	return true
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]core.ModelMetadata, error) {
	// A simple list for now
	return []core.ModelMetadata{
		{ID: "gpt-4o", Context: 128000},
		{ID: "gpt-4o-mini", Context: 128000},
	}, nil
}

func (p *OpenAIProvider) CreateClient(modelID string, opts ClientOptions) (core.LLMClient, error) {
	c := llm.NewClient(opts.BaseURL, opts.APIKey, modelID)
	c.SetProvider(p.id)
	return c, nil
}

func (p *OpenAIProvider) Close() error {
	return nil
}
