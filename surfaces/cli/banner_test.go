package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/darkcode/infra/config"
	"github.com/darkcode/memory/memory"
	"github.com/darkcode/tools/tools"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever it wrote. printBanner writes with fmt.Println directly rather
// than to an io.Writer, so this is the only way to assert on its output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

// TestBannerDoesNotOverclaimAutoHealing is the regression test for Phase 7's
// banner fix: "Auto-Healing Loop" named kernel/selfheal as if it ran on its
// own schedule, but it is on-demand-tool-only (see kernel/selfheal and the
// project's own explicit non-goal against building an autonomous daemon).
// The banner must name what actually runs instead.
func TestBannerDoesNotOverclaimAutoHealing(t *testing.T) {
	mem, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("memory.NewSystem: %v", err)
	}
	t.Cleanup(mem.Shutdown)

	out := captureStdout(t, func() {
		printBanner(&config.Config{}, mem, tools.NewRegistry(), nil)
	})

	if strings.Contains(out, "Auto-Healing Loop") {
		t.Error("banner still claims \"Auto-Healing Loop\" — kernel/selfheal is on-demand, not autonomous")
	}
	if !strings.Contains(out, "Verifier-Gated Repair") {
		t.Error("banner does not name what actually runs (verifier-gated repair)")
	}
}

// TestBannerMemoryLayerCountMatchesTierNames is the regression test for the
// three-different-counts finding: the capability line's "N-Layer Memory"
// and the L4 architecture description must both be built from
// memory.TierNames(), not a separately hardcoded number that can drift from
// it (or from memory.System.Summary(), which enumerates the same list).
func TestBannerMemoryLayerCountMatchesTierNames(t *testing.T) {
	mem, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("memory.NewSystem: %v", err)
	}
	t.Cleanup(mem.Shutdown)

	out := captureStdout(t, func() {
		printBanner(&config.Config{}, mem, tools.NewRegistry(), nil)
	})

	names := memory.TierNames()
	wantCount := len(names)
	if wantCount == 0 {
		t.Fatal("memory.TierNames() is empty — nothing to compare the banner against")
	}
	if !strings.Contains(out, "7-Layer Memory") && wantCount == 7 {
		// Pinned to the current canonical count so a silent change to
		// TierNames() (adding/removing a store) is caught here rather than
		// only in the banner a human happens to read.
		t.Errorf("banner capability line does not show %d-Layer Memory (TierNames() has %d entries)", wantCount, wantCount)
	}

	wantDesc := strings.ToLower(strings.Join(names, " · "))
	if !strings.Contains(out, wantDesc) {
		t.Errorf("banner's L4 description does not match memory.TierNames(); want it to contain %q", wantDesc)
	}
}
