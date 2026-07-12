package provider

import (
	"context"

	"github.com/darkcode/core"
)

type ProviderType int

const (
	ProviderEmbedded ProviderType = iota // llama.cpp in-process
	ProviderLocal                        // Ollama, LM Studio (separate process)
	ProviderCloud                        // OpenAI, Anthropic, etc.
)

// ClientOptions contains configuration for creating a new client.
type ClientOptions struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Provider represents a model provider backend.
type Provider interface {
	ID() string
	Name() string
	Type() ProviderType
	IsAvailable(ctx context.Context) bool
	ListModels(ctx context.Context) ([]core.ModelMetadata, error)
	CreateClient(modelID string, opts ClientOptions) (core.LLMClient, error)
	Close() error
}
