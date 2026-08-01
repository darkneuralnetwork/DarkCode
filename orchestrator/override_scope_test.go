package orchestrator

import (
	"sync"
	"testing"

	"github.com/darkcode/core"
)

// A verb applies to one message. The mechanism is save-old, set-new, restore on
// return — but the thing being saved and restored is shared router state, so
// two overlapping requests can interleave their saves and leave the router in a
// mode nobody asked for.
//
// It matters most for /consensus: stuck in consensus means every later query
// fans out to every registered model, which on a metered tier is the most
// expensive way to be wrong.

func TestOneVerbAffectsOneMessage(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)
	base := deps.Router.GetMode()

	restore := deps.Kernel.ApplyRequestOverrides("consensus", "", "", "", "")
	if got := deps.Router.GetMode(); got != core.RouteConsensus {
		t.Fatalf("mode during the request = %v, want consensus", got)
	}
	restore()

	if got := deps.Router.GetMode(); got != base {
		t.Errorf("mode after the request = %v, want the configured %v — the verb "+
			"leaked into every later query", got, base)
	}
}

// TestOverlappingVerbsDoNotStickTheRouter is the interleaving. Request A saves
// "single" and sets consensus; B then saves *consensus* as its base and sets
// consensus; A restores single; B restores consensus — and the router is stuck
// there with no request asking for it.
func TestOverlappingVerbsDoNotStickTheRouter(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)
	base := deps.Router.GetMode()

	restoreA := deps.Kernel.ApplyRequestOverrides("consensus", "", "", "", "")
	restoreB := deps.Kernel.ApplyRequestOverrides("consensus", "", "", "", "")
	restoreA()
	restoreB()

	if got := deps.Router.GetMode(); got != base {
		t.Errorf("after two overlapping /consensus requests the router is %v, want %v — "+
			"every later query now fans out to all models", got, base)
	}
}

// TestConcurrentOverridesAlwaysReturnToBase under -race.
func TestConcurrentOverridesAlwaysReturnToBase(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)
	base := deps.Router.GetMode()

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		mode := "consensus"
		if i%3 == 0 {
			mode = "escalation"
		}
		go func() {
			defer wg.Done()
			restore := deps.Kernel.ApplyRequestOverrides(mode, "", "", "", "")
			restore()
		}()
	}
	wg.Wait()

	if got := deps.Router.GetMode(); got != base {
		t.Errorf("after concurrent verb requests the router settled on %v, want %v", got, base)
	}
}

// TestOverlappingSafetyOverridesDoNotStick. The same interleaving on the
// permission gate is worse than on the router: it would leave the gate at a
// level nobody chose, in either direction.
func TestOverlappingSafetyOverridesDoNotStick(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)
	base := deps.Kernel.Gate().Level()

	a := deps.Kernel.ApplyRequestOverrides("", "relaxed", "", "", "")
	b := deps.Kernel.ApplyRequestOverrides("", "relaxed", "", "", "")
	a()
	b()

	if got := deps.Kernel.Gate().Level(); got != base {
		t.Errorf("the gate settled on %v after two overlapping requests, want %v — "+
			"a relaxed level that outlives its request is a standing grant", got, base)
	}
}

// TestNestedOverridesRestoreInAnyOrder. Requests finish when their work does,
// not in the order they started.
func TestNestedOverridesRestoreInAnyOrder(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ok"}}
	deps := newTestKernel(t, client)
	base := deps.Router.GetMode()

	a := deps.Kernel.ApplyRequestOverrides("consensus", "", "", "", "")
	b := deps.Kernel.ApplyRequestOverrides("escalation", "", "", "", "")
	c := deps.Kernel.ApplyRequestOverrides("consensus", "", "", "", "")

	b() // finishes first
	c()
	a()

	if got := deps.Router.GetMode(); got != base {
		t.Errorf("out-of-order completion left the router at %v, want %v", got, base)
	}
}
