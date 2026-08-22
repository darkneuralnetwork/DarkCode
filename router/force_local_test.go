package router

import (
	"testing"

	"github.com/darkcode/core"
)

// TestForceLocal_SelectsLocalIgnoringCloud asserts that with force-local set,
// a request that would normally route to a cloud tier (coding) is served by
// the registered local model instead.
func TestForceLocal_SelectsLocalIgnoringCloud(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "cloud-coding"}, "cloud-coding")
	r.MarkPrimary("cloud-coding")
	r.RegisterModel(core.ModelTierMediumLocal, &fakeClient{name: "local-medium"}, "local-medium")

	r.SetForceLocal(true)

	_, name, err := r.Route(core.ModelTierCoding, 3, "explain this code")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if name != "local-medium" {
		t.Fatalf("force-local must select the local model, got %q", name)
	}
}

// TestForceLocal_NeverFallsBackToCloud is the core guarantee: when force-local
// is active and NO local model is registered, Route errors instead of
// silently serving the available cloud model.
func TestForceLocal_NeverFallsBackToCloud(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "cloud-coding"}, "cloud-coding")
	r.MarkPrimary("cloud-coding")

	r.SetForceLocal(true)

	_, name, err := r.Route(core.ModelTierCoding, 3, "explain this code")
	if err == nil {
		t.Fatalf("force-local with no local model must error, but got model %q", name)
	}
}

// TestForceLocal_OverridesReasoningPreference asserts force-local wins even
// for a reasoning-tier request (the prefer-local advisor path deliberately
// skips reasoning/critic, but force-local must not).
func TestForceLocal_OverridesReasoningPreference(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierReasoning, &fakeClient{name: "cloud-reasoning"}, "cloud-reasoning")
	r.RegisterModel(core.ModelTierTinyLocal, &fakeClient{name: "local-tiny"}, "local-tiny")
	r.MarkPrimary("cloud-reasoning")

	r.SetForceLocal(true)

	_, name, err := r.Route(core.ModelTierReasoning, 9, "hard reasoning task")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if name != "local-tiny" {
		t.Fatalf("force-local must override a reasoning request, got %q", name)
	}
}

// TestForceLocal_ToggleOffRestoresCloud confirms SetForceLocal(false) unpins
// routing — the cloud model becomes selectable again with no restart.
func TestForceLocal_ToggleOffRestoresCloud(t *testing.T) {
	r := NewRouter(core.RouteSingle, nil)
	r.RegisterModel(core.ModelTierCoding, &fakeClient{name: "cloud-coding"}, "cloud-coding")
	r.MarkPrimary("cloud-coding")

	r.SetForceLocal(true)
	if _, _, err := r.Route(core.ModelTierCoding, 3, "task"); err == nil {
		t.Fatal("precondition: force-local with no local model should error")
	}

	r.SetForceLocal(false)
	if r.ForceLocal() {
		t.Fatal("ForceLocal() should report false after SetForceLocal(false)")
	}
	_, name, err := r.Route(core.ModelTierCoding, 3, "task")
	if err != nil {
		t.Fatalf("after unpinning, Route should succeed: %v", err)
	}
	if name != "cloud-coding" {
		t.Fatalf("expected cloud model after unpinning, got %q", name)
	}
}

// TestForceLocal_OffloadInterceptStillLocal asserts the task-offload intercept
// (which selects tiny/medium-local directly) still honors force-local — i.e.
// it can't accidentally reach cloud either.
func TestForceLocal_ConsensusRestrictedToLocal(t *testing.T) {
	r := NewRouter(core.RouteConsensus, nil)
	r.RegisterModel(core.ModelTierReasoning, &fakeClient{name: "cloud-reasoning"}, "cloud-reasoning")
	r.MarkPrimary("cloud-reasoning")

	r.SetForceLocal(true)

	// No local model registered → consensus must refuse, not consult cloud.
	if _, err := r.Consensus(nil, nil, "goal"); err == nil {
		t.Fatal("force-local consensus with no local model must error")
	}
}
