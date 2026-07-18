package orchestrator

import "testing"

// TestApplyRequestOverrides_Brain verifies the per-request brain selector:
// "local" pins routing to the local model and the restore closure reverts it;
// "cloud" unpins; "" leaves it unchanged.
func TestApplyRequestOverrides_Brain(t *testing.T) {
	deps := newTestKernel(t, nil)
	r := deps.Router

	// Baseline.
	if r.ForceLocal() {
		t.Fatal("expected forceLocal=false at baseline")
	}

	// local → pin to local, then restore.
	restore := deps.Kernel.ApplyRequestOverrides("", "", "", "", "local")
	if !r.ForceLocal() {
		t.Fatal("brain=local should pin forceLocal=true")
	}
	restore()
	if r.ForceLocal() {
		t.Fatal("restore should revert forceLocal to false")
	}

	// Pin via config, then a cloud request should unpin for that request only.
	r.SetForceLocal(true)
	restore = deps.Kernel.ApplyRequestOverrides("", "", "", "", "cloud")
	if r.ForceLocal() {
		t.Fatal("brain=cloud should unpin forceLocal for the request")
	}
	restore()
	if !r.ForceLocal() {
		t.Fatal("restore should revert forceLocal to the pinned config value")
	}
	r.SetForceLocal(false)

	// empty brain leaves forceLocal unchanged.
	restore = deps.Kernel.ApplyRequestOverrides("", "", "", "", "")
	if r.ForceLocal() {
		t.Fatal("brain='' should not change forceLocal")
	}
	restore()
}
