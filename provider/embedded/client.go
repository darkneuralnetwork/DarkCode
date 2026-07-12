package embedded

// client.go — EmbeddedClient wraps the standard llm.Client to talk to
// llama-server's OpenAI-compatible API (/v1/chat/completions). Since
// llama-server speaks the OpenAI protocol, we don't need a custom HTTP
// client — we reuse the existing llm.Client with AuthScheme "none".
//
// This is the bridge between the ProcessManager (which spawns llama-server)
// and the core.LLMClient interface (which the kernel/router use for chat).
//
// Model-swap guard: each EmbeddedClient captures the Provider generation
// counter at creation time and fails fast (errModelSwapped) on any call made
// after the running model has been swapped out from under it. This prevents a
// retry-storm when a model is hot-swapped mid-request.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/darkcode/core"
	"github.com/darkcode/llm"
)

// errModelSwapped is returned when an in-flight client's model was swapped
// out by a later LoadModel call. RetryingClient surfaces it to the worker,
// which falls back instead of retrying against a dead server.
var errModelSwapped = errors.New("embedded: model was swapped during request")

// EmbeddedClient wraps an llm.Client pointed at the local llama-server.
type EmbeddedClient struct {
	*llm.Client
	modelID string
	prov    *Provider
	// genAtCreate is the Provider.generation captured when this client was
	// created. If the provider's current generation differs, the model has
	// been swapped and this client must fail fast.
	genAtCreate uint64
}

// NewEmbeddedClient creates a client for the local llama-server instance.
// baseURL is the /v1 endpoint (e.g. http://127.0.0.1:PORT/v1).
func NewEmbeddedClient(baseURL, providerID, modelID string) *EmbeddedClient {
	// llama-server's OpenAI endpoint needs no API key.
	c := llm.NewClient(baseURL, "no-key-required", modelID)
	c.Provider = providerID
	c.AuthScheme = "none"
	ec := &EmbeddedClient{Client: c, modelID: modelID}
	// Attach to the singleton provider so we can guard against model swaps.
	// If the singleton isn't initialized (e.g. unit test), genAtCreate stays
	// 0 and the guard is a no-op (Generation() also returns 0).
	ec.prov = Default()
	ec.genAtCreate = ec.prov.Generation()
	return ec
}

// checkGeneration fails fast if the underlying model has been swapped since
// this client was created. Returns errModelSwapped in that case.
func (e *EmbeddedClient) checkGeneration() error {
	if e.prov == nil {
		return nil
	}
	if atomic.LoadUint64(&e.prov.generation) != e.genAtCreate {
		return errModelSwapped
	}
	return nil
}

// ChatCompletion proxies to llama-server, but first verifies the model has
// not been swapped out from under this client.
func (e *EmbeddedClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	if err := e.checkGeneration(); err != nil {
		return nil, err
	}
	if e.prov != nil {
		e.prov.touch()
	}
	return e.Client.ChatCompletion(ctx, req)
}

// ChatCompletionStream proxies to llama-server, but first verifies the model
// has not been swapped out from under this client.
func (e *EmbeddedClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *llm.StreamCallbacks) (*core.CompletionResponse, error) {
	if err := e.checkGeneration(); err != nil {
		return nil, err
	}
	if e.prov != nil {
		e.prov.touch()
	}
	return e.Client.ChatCompletionStream(ctx, req, cb)
}

// CreateEmbedding proxies to llama-server.
func (e *EmbeddedClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	if err := e.checkGeneration(); err != nil {
		return nil, err
	}
	if e.prov != nil {
		e.prov.touch()
	}
	return e.Client.CreateEmbedding(ctx, text)
}

// ModelInfo returns metadata for the loaded embedded model, including its
// actual configured context window (was previously hardcoded to 4096
// regardless of the real -c value, which could be up to 131072 — this
// silently undercut any caller that budgets context from ModelInfo().Context,
// e.g. the ctxengine integration in orchestrator.Kernel.getCtxEngine()).
// Falls back to 0 (not a guessed constant) if no provider is attached or no
// model has loaded yet; callers that budget tokens already treat <=0 as
// "use my own sensible default" (see ctxengine.Engine.Assemble).
func (e *EmbeddedClient) ModelInfo() core.ModelMetadata {
	ctxSize := 0
	if e.prov != nil {
		ctxSize = e.prov.ContextSize()
	}
	return core.ModelMetadata{
		ID:      e.modelID,
		Context: ctxSize,
	}
}

// GuardedClient is exported for tests that need to assert the swap-guard
// behavior without spinning up a real llama-server.
func NewEmbeddedClientForTest(prov *Provider, baseURL, modelID string) *EmbeddedClient {
	c := llm.NewClient(baseURL, "no-key-required", modelID)
	c.AuthScheme = "none"
	ec := &EmbeddedClient{Client: c, modelID: modelID, prov: prov}
	ec.genAtCreate = prov.Generation()
	return ec
}

// MountLoRA dynamically sets the scale of a LoRA adapter by name.
// This implements core.LoRAManager.
func (e *EmbeddedClient) MountLoRA(name string, scale float32) error {
	if e.prov == nil || e.prov.pm == nil {
		return fmt.Errorf("no process manager available for dynamic lora")
	}
	id, ok := e.prov.pm.GetLoRAID(name)
	if !ok {
		return fmt.Errorf("lora %q not found or not loaded at startup", name)
	}
	return e.prov.pm.SetLoRAScale(context.Background(), id, scale)
}

var _ = fmt.Sprintf
