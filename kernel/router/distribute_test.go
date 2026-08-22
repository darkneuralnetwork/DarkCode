package router

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
)

type stubClient struct{ name string }

func (s *stubClient) ChatCompletion(context.Context, *core.CompletionRequest) (*core.CompletionResponse, error) {
	return &core.CompletionResponse{}, nil
}
func (s *stubClient) ChatCompletionStream(context.Context, *core.CompletionRequest, *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return &core.CompletionResponse{}, nil
}
func (s *stubClient) CreateEmbedding(context.Context, string) ([]float32, error) { return nil, nil }
func (s *stubClient) ModelInfo() core.ModelMetadata                              { return core.ModelMetadata{ID: s.name} }
func (s *stubClient) Ping(context.Context) error                                 { return nil }
func (s *stubClient) Close() error                                               { return nil }

// TestRouteWorkerSpreadsAcrossModels is the point of distribution. The executor
// already ran a wave of independent tasks concurrently; every one of them
// resolved to the same tier client, so they were parallel in the executor and
// serial at the provider.
func TestRouteWorkerSpreadsAcrossModels(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierReasoning, &stubClient{name: "primary"}, "primary")
	r.RegisterModel(core.ModelTierCoding, &stubClient{name: "helper-a"}, "helper-a")
	r.RegisterModel(core.ModelTierFast, &stubClient{name: "helper-b"}, "helper-b")
	r.MarkPrimary("primary")

	seen := map[string]bool{}
	for slot := 0; slot < 4; slot++ {
		_, name, err := r.RouteWorker(core.ModelTierCoding, 5, "build a piece", slot)
		if err != nil {
			t.Fatalf("RouteWorker(slot %d): %v", slot, err)
		}
		seen[name] = true
	}
	if len(seen) < 2 {
		t.Errorf("a 4-task wave used %d distinct model(s) (%v); distribution should spread it", len(seen), seen)
	}
}

// TestRouteWorkerSlotZeroIsUnchanged. A single-task wave, and the first task of
// any wave, must route exactly as before — the primary stays on the critical
// path and a one-model install sees no behaviour change at all.
func TestRouteWorkerSlotZeroIsUnchanged(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierReasoning, &stubClient{name: "primary"}, "primary")
	r.RegisterModel(core.ModelTierCoding, &stubClient{name: "helper"}, "helper")
	r.MarkPrimary("primary")

	_, viaRoute, err := r.Route(core.ModelTierCoding, 5, "task")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	_, viaWorker, err := r.RouteWorker(core.ModelTierCoding, 5, "task", 0)
	if err != nil {
		t.Fatalf("RouteWorker: %v", err)
	}
	if viaRoute != viaWorker {
		t.Errorf("slot 0 routed to %q but Route chose %q — they must agree", viaWorker, viaRoute)
	}
}

// TestRouteWorkerSingleModelStillWorks: with nothing to spread across,
// distribution must fall back rather than fail.
func TestRouteWorkerSingleModelStillWorks(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &stubClient{name: "only"}, "only")
	r.MarkPrimary("only")

	for slot := 0; slot < 3; slot++ {
		_, name, err := r.RouteWorker(core.ModelTierCoding, 5, "task", slot)
		if err != nil {
			t.Fatalf("RouteWorker(slot %d): %v", slot, err)
		}
		if name != "only" {
			t.Errorf("slot %d = %q, want the single registered model", slot, name)
		}
	}
}

// TestRouteWorkerPrefersSupporters. A model registered specifically to take a
// share of the work should be reached for before one whose real job is
// something else.
func TestRouteWorkerPrefersSupporters(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierReasoning, &stubClient{name: "primary"}, "primary")
	r.RegisterModel(core.ModelTierCoding, &stubClient{name: "critic-model"}, "critic-model")
	r.RegisterModel(core.ModelTierFast, &stubClient{name: "helper"}, "helper")
	r.MarkPrimary("primary")
	r.SetModelRole("critic-model", "critic")
	r.SetModelRole("helper", RoleSupporter)

	_, name, err := r.RouteWorker(core.ModelTierCoding, 5, "task", 1)
	if err != nil {
		t.Fatalf("RouteWorker: %v", err)
	}
	if name != "helper" {
		t.Errorf("slot 1 = %q, want the supporter-registered model", name)
	}
}

// TestRouteWorkerHonoursForceLocal. Distribution must not become a way around
// the force-local guarantee Route makes.
func TestRouteWorkerHonoursForceLocal(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierLocal, &stubClient{name: "local"}, "local")
	r.RegisterModel(core.ModelTierReasoning, &stubClient{name: "cloud"}, "cloud")
	r.MarkPrimary("local")
	r.SetForceLocal(true)

	for slot := 0; slot < 4; slot++ {
		_, name, err := r.RouteWorker(core.ModelTierCoding, 5, "task", slot)
		if err != nil {
			continue // no local tier for this request is a legitimate refusal
		}
		if name == "cloud" {
			t.Fatalf("slot %d reached a cloud model while force-local was on", slot)
		}
	}
}
