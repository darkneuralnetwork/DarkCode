package cli

import (
	"path/filepath"
	"testing"

	"github.com/darkcode/config"
	"github.com/darkcode/verb"
)

// The setters are the console's only write path into configuration. Their job
// is to reject nonsense before it reaches disk — a setter that accepts "yes"
// as a safety level stores a value nothing downstream understands.
//
// These tests point DARKCODE_CONFIG at a temp file, so Save writes there
// instead of the developer's real config. Without that override this whole
// file could not exist, which is most of why the package had no coverage.

func newSettingsConsole(t *testing.T) *Console {
	t.Helper()
	t.Setenv("DARKCODE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	return &Console{cfg: config.DefaultConfig()}
}

func TestSetAlwaysTakesVerbsAndOff(t *testing.T) {
	c := newSettingsConsole(t)

	for _, name := range verb.Names() {
		c.setAlways(name)
		if c.stickyVerb != name {
			t.Errorf("setAlways(%q) left stickyVerb = %q", name, c.stickyVerb)
		}
	}

	// A leading slash is how people actually type it.
	c.setAlways("/graph")
	if c.stickyVerb != "graph" {
		t.Errorf("setAlways(\"/graph\") = %q, want graph", c.stickyVerb)
	}

	for _, off := range []string{"off", "auto", "none", "OFF"} {
		c.setAlways("loop")
		c.setAlways(off)
		if c.stickyVerb != "" {
			t.Errorf("setAlways(%q) left stickyVerb = %q, want cleared", off, c.stickyVerb)
		}
	}
}

// TestSetAlwaysRejectsAnUnknownVerb. Storing a verb nothing can look up means
// every later message silently falls through to the default.
func TestSetAlwaysRejectsAnUnknownVerb(t *testing.T) {
	c := newSettingsConsole(t)
	c.setAlways("graph")

	c.setAlways("teleport")
	if c.stickyVerb != "graph" {
		t.Errorf("an unknown verb changed the setting to %q", c.stickyVerb)
	}
	// And it must not accept the old chat/build/loop vocabulary either — that
	// is the ambiguity /always was introduced to remove.
	c.setAlways("build")
	if c.stickyVerb != "graph" {
		t.Errorf("the retired chat-mode vocabulary was accepted: %q", c.stickyVerb)
	}
}

func TestSetBrainValidates(t *testing.T) {
	c := newSettingsConsole(t)

	for _, good := range []string{"auto", "local", "cloud"} {
		c.brain = ""
		c.setBrain(good)
		if c.brain != good {
			t.Errorf("setBrain(%q) = %q", good, c.brain)
		}
	}

	c.setBrain("local")
	for _, bad := range []string{"quantum", "", "LOCAL-ISH"} {
		c.setBrain(bad)
		if c.brain != "local" {
			t.Errorf("setBrain(%q) changed the brain to %q", bad, c.brain)
		}
	}
}

func TestSetBackgroundWorkValidates(t *testing.T) {
	c := newSettingsConsole(t)

	for _, level := range []string{"off", "light", "full"} {
		c.setBackgroundWork(level)
		if got := c.cfg.ResolvedBackgroundWork(); got != level {
			t.Errorf("setBackgroundWork(%q) resolved to %q", level, got)
		}
	}

	c.setBackgroundWork("full")
	for _, bad := range []string{"maximum", "on", "yes", ""} {
		c.setBackgroundWork(bad)
		if got := c.cfg.ResolvedBackgroundWork(); got != "full" {
			t.Errorf("setBackgroundWork(%q) changed the level to %q", bad, got)
		}
	}
}

func TestSetMemoryProfileValidates(t *testing.T) {
	c := newSettingsConsole(t)

	for _, p := range []string{"lean", "balanced", "max"} {
		c.setMemoryProfile(p)
		if c.cfg.MemoryProfile != p {
			t.Errorf("setMemoryProfile(%q) = %q", p, c.cfg.MemoryProfile)
		}
	}
	// "auto" is spelled as the empty value internally.
	c.setMemoryProfile("auto")
	if c.cfg.MemoryProfile != "" {
		t.Errorf("auto stored as %q, want empty", c.cfg.MemoryProfile)
	}

	c.setMemoryProfile("max")
	for _, bad := range []string{"huge", "16k", "unlimited"} {
		c.setMemoryProfile(bad)
		if c.cfg.MemoryProfile != "max" {
			t.Errorf("setMemoryProfile(%q) changed it to %q", bad, c.cfg.MemoryProfile)
		}
	}
}

func TestSetSandboxValidates(t *testing.T) {
	c := newSettingsConsole(t)

	for _, m := range []string{"off", "auto", "on", "strict"} {
		c.setSandbox(m)
		if c.cfg.Sandbox != m {
			t.Errorf("setSandbox(%q) = %q", m, c.cfg.Sandbox)
		}
	}

	c.setSandbox("strict")
	for _, bad := range []string{"maybe", "true", "1"} {
		c.setSandbox(bad)
		if c.cfg.Sandbox != "strict" {
			t.Errorf("setSandbox(%q) weakened the sandbox to %q", bad, c.cfg.Sandbox)
		}
	}
}

// TestSetLocalWritesOnlyLocalMode. enable_local_llm is the legacy half of a
// two-field question; writing both is how they came to disagree.
func TestSetLocalWritesOnlyLocalMode(t *testing.T) {
	c := newSettingsConsole(t)
	c.cfg.EnableLocalLLM = false

	c.setLocal([]string{"force"})
	if c.cfg.LocalMode != "force" {
		t.Errorf("local_mode = %q, want force", c.cfg.LocalMode)
	}
	if c.cfg.EnableLocalLLM {
		t.Error("setLocal wrote the legacy enable_local_llm field")
	}
	if !c.cfg.LocalEnabled() {
		t.Error("force mode does not report local as enabled")
	}

	c.setLocal([]string{"off"})
	if c.cfg.LocalMode != "off" || c.cfg.LocalEnabled() {
		t.Errorf("off left local_mode=%q enabled=%v", c.cfg.LocalMode, c.cfg.LocalEnabled())
	}

	c.setLocal([]string{"sideways"})
	if c.cfg.LocalMode != "off" {
		t.Errorf("an invalid state changed local_mode to %q", c.cfg.LocalMode)
	}
}

// TestSettersPersist. A setting that only lives in memory is lost on the next
// start, which reads as the setter not having worked.
func TestSettersPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("DARKCODE_CONFIG", path)

	c := &Console{cfg: config.DefaultConfig()}
	c.setBackgroundWork("light")
	c.setSandbox("strict")

	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.ResolvedBackgroundWork(); got != "light" {
		t.Errorf("background_work did not persist: %q", got)
	}
	if reloaded.Sandbox != "strict" {
		t.Errorf("sandbox did not persist: %q", reloaded.Sandbox)
	}
}

// TestRenderValueShowsAbsence. An unset setting rendered as blank space is
// indistinguishable from one this renderer failed to read.
func TestRenderValueShowsAbsence(t *testing.T) {
	cases := map[string]any{
		"—":            nil,
		"on":           true,
		"off":          false,
		"42":           float64(42),
		"0.25":         float64(0.25),
		"a, b":         []any{"a", "b"},
		"2 configured": map[string]any{"x": 1, "y": 2},
	}
	for want, in := range cases {
		if got := renderValue(in); got != want {
			t.Errorf("renderValue(%v) = %q, want %q", in, got, want)
		}
	}
	// Empty collections and strings all read as absent, not as blank.
	for _, empty := range []any{"", []any{}, map[string]any{}} {
		if got := renderValue(empty); got != "—" {
			t.Errorf("renderValue(%v) = %q, want the absent marker", empty, got)
		}
	}
}
