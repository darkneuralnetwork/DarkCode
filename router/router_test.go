package router

import (
	"context"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// fakeClient is a minimal named core.LLMClient for router-level tests — it
// doesn't need to produce realistic completions, only to be distinguishable
// by name so tests can assert WHICH model was selected.
type fakeClient struct{ name string }

func (f *fakeClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: f.name}}}}, nil
}
func (f *fakeClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return f.ChatCompletion(ctx, req)
}
func (f *fakeClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (f *fakeClient) ModelInfo() core.ModelMetadata                                       { return core.ModelMetadata{ID: f.name} }
func (f *fakeClient) Ping(ctx context.Context) error                                      { return nil }
func (f *fakeClient) Close() error                                                        { return nil }

func TestDisableModel_RouteSkipsDisabledModel(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "primary"}, "primary")
	r.MarkPrimary("primary")
	r.RegisterModel(core.ModelTierFast, &fakeClient{name: "fast-model"}, "fast-model")

	r.DisableModel("primary", time.Now().Add(time.Hour))

	_, name, err := r.Route(core.ModelTierCoding, 3, "do something")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if name == "primary" {
		t.Errorf("Route selected the disabled model %q, want it to fall through to another tier", name)
	}
}

func TestDisableModel_ErrorsWhenEverythingDisabled(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "only-model"}, "only-model")
	r.MarkPrimary("only-model")

	r.DisableModel("only-model", time.Now().Add(time.Hour))

	if _, _, err := r.Route(core.ModelTierCoding, 3, "do something"); err == nil {
		t.Error("expected Route to fail when the only registered model is disabled")
	}
}

func TestEnableModel_ReversesDisable(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "only-model"}, "only-model")
	r.MarkPrimary("only-model")
	r.DisableModel("only-model", time.Now().Add(time.Hour))

	r.EnableModel("only-model")

	if _, name, err := r.Route(core.ModelTierCoding, 3, "do something"); err != nil || name != "only-model" {
		t.Errorf("Route after EnableModel = (%q, %v), want (only-model, nil)", name, err)
	}
}

func TestDisableModel_LazyExpiry(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "only-model"}, "only-model")
	r.MarkPrimary("only-model")

	// Disabled until a time already in the past — should behave as enabled
	// without any explicit EnableModel call (lazy expiry).
	r.DisableModel("only-model", time.Now().Add(-time.Minute))

	if r.IsModelDisabled("only-model") {
		t.Error("a DisabledUntil in the past must report as enabled")
	}
	if _, _, err := r.Route(core.ModelTierCoding, 3, "do something"); err != nil {
		t.Errorf("Route: %v, want the lazily-expired model to be selectable", err)
	}
}

func TestIsModelDisabled_UnknownModelIsFalse(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	if r.IsModelDisabled("does-not-exist") {
		t.Error("an unregistered model must report as not disabled")
	}
}

func TestDisableModel_ConsensusExcludesDisabledContributor(t *testing.T) {
	r := NewRouter(core.RouteConsensus, nil)
	r.RegisterModel(core.ModelTierReasoning, &fakeClient{name: "primary"}, "primary")
	r.MarkPrimary("primary")
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "contributor-a"}, "contributor-a")
	r.RegisterModel(core.ModelTierFast, &fakeClient{name: "contributor-b"}, "contributor-b")

	r.DisableModel("contributor-a", time.Now().Add(time.Hour))

	result, err := r.Consensus(context.Background(), []core.Message{{Role: core.RoleUser, Content: "hi"}}, "hi")
	if err != nil {
		t.Fatalf("Consensus: %v", err)
	}
	for _, c := range result.Contributions {
		if c.Model == "contributor-a" {
			t.Error("disabled model contributor-a must not appear in the consensus contributions")
		}
	}
}
