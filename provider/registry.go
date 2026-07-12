package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/darkcode/core"
)

// Registry manages available model providers.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates a new provider registry with built-in providers.
func NewRegistry() *Registry {
	r := &Registry{
		providers: make(map[string]Provider),
	}
	// Register default providers
	r.Register(NewOpenAIProvider())
	r.Register(NewOllamaProvider(""))
	r.Register(NewLMStudioProvider(""))
	return r
}

// Register adds a provider to the registry.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.ID()] = p
}

// Get retrieves a provider by ID.
func (r *Registry) Get(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// ListAvailable returns all providers that are currently available.
func (r *Registry) ListAvailable(ctx context.Context) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	var available []Provider
	for _, p := range r.providers {
		if p.IsAvailable(ctx) {
			available = append(available, p)
		}
	}
	return available
}

// CreateClient creates an LLMClient from the given provider and model.
func (r *Registry) CreateClient(providerID string, modelID string, opts ClientOptions) (core.LLMClient, error) {
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("provider %q not found", providerID)
	}

	return p.CreateClient(modelID, opts)
}
