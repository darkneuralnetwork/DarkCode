//go:build !llamacpp

package embedded

import (
	"testing"
	"time"
)

// TestProviderCloseStopsIdleMonitor guards against the goroutine leak where
// Close() left the idle-monitor goroutine running forever (it only stopped
// the health loop via pm.Stop). A leaked monitor keeps ticking and could even
// unload a later-reloaded model. Close() must terminally stop it.
func TestProviderCloseStopsIdleMonitor(t *testing.T) {
	p := &Provider{pm: NewProcessManager("")}
	stop := make(chan struct{})
	p.idleStop = stop

	exited := make(chan struct{})
	go func() {
		p.idleMonitor(stop)
		close(exited)
	}()

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-exited:
		// good — monitor observed the closed stop channel and returned.
	case <-time.After(2 * time.Second):
		t.Fatal("idle-monitor goroutine did not exit after Close() — leak")
	}

	if p.idleStop != nil {
		t.Error("Close() should nil out idleStop after closing it")
	}
}

// TestProviderUnloadKeepsIdleMonitorAlive verifies the intended distinction:
// a normal UnloadModel (e.g. an idle-triggered unload) must NOT stop the
// monitor, so it can resume watching when a new model loads.
func TestProviderUnloadKeepsIdleMonitorAlive(t *testing.T) {
	p := &Provider{pm: NewProcessManager("")}
	stop := make(chan struct{})
	p.idleStop = stop

	p.UnloadModel()

	select {
	case <-stop:
		t.Fatal("UnloadModel must not close idleStop — the monitor should survive a normal unload")
	default:
		// good — still open.
	}
	if p.idleStop == nil {
		t.Error("UnloadModel should leave idleStop intact")
	}
}
