package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/darkcode/config"
	"github.com/darkcode/security"
	"github.com/darkcode/verb"
)

// printConfig renders the settings from the descriptors on the config type
// rather than from a list maintained here.
//
// The hand-written version showed eleven fields it had picked — a different
// subset from the API's nineteen and the Settings tab's sixteen, so "what can I
// configure" had a different answer in each place. Rendering the shared
// descriptors means the console cannot fall behind again.
func (c *Console) printConfig() {
	values := config.Values(c.cfg)
	fmt.Println(paint(cAmber+clrBold, "CONFIGURATION"))

	tiers := []struct {
		tier  config.Tier
		title string
		note  string
	}{
		{config.TierPrimary, "", ""},
		{config.TierAdvanced, "ADVANCED", "overrides; the defaults are right for almost everyone"},
		{config.TierDerived, "DERIVED", "computed, not set"},
	}
	for _, t := range tiers {
		fields := config.FieldsInTier(t.tier)
		if len(fields) == 0 {
			continue
		}
		if t.title != "" {
			fmt.Printf("\n  %s %s\n", paint(cGray+clrBold, t.title), paint(cGray, "— "+t.note))
		}
		group := ""
		for _, f := range fields {
			if f.Group != group {
				group = f.Group
				fmt.Printf("  %s\n", paint(cBlue, group))
			}
			fmt.Printf("    %-30s %s\n", paint(cGray, f.Name), paint(cWhite, renderValue(values[f.Name])))
		}
	}

	if len(c.cfg.Models) > 0 {
		fmt.Printf("\n  %s\n", paint(cBlue, "registered models"))
		for k, m := range c.cfg.Models {
			primary := ""
			if k == c.cfg.Model {
				primary = paint(cOrange, " (primary)")
			}
			fmt.Printf("    • %s  %s  %s%s\n", paint(cWhite, m.Model), paint(cGray, m.Provider), paint(cGray, m.BaseURL), primary)
		}
	}
}

// renderValue prints a setting's value, showing an unset one as a dash rather
// than as blank space — absence should be visible, not inferred from a gap.
func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		if x == "" {
			return "—"
		}
		return x
	case bool:
		if x {
			return "on"
		}
		return "off"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case []any:
		if len(x) == 0 {
			return "—"
		}
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		if len(x) == 0 {
			return "—"
		}
		return fmt.Sprintf("%d configured", len(x))
	default:
		return fmt.Sprint(x)
	}
}

func (c *Console) setModel(name string) {
	c.cfg.Model = name
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	fmt.Printf("%s model set to %s %s\n", paint(cGreen, "✓"), paint(cOrange+clrBold, name), paint(cGray, "(hot-reloaded)"))
}

// setCompressor selects the model used for ALL context compression (STM +
// project summary). "primary" (or empty) clears the override and uses the
// primary model. Hot-reloaded via ReloadModels so the change takes effect
// immediately. Mirrors setModel/setMode.
func (c *Console) setCompressor(name string) {
	name = strings.TrimSpace(name)
	// "primary" means: no dedicated compressor — use the primary model.
	if name == "primary" || name == "" {
		c.cfg.CompressorModel = ""
	} else if _, ok := c.cfg.Models[name]; !ok {
		fmt.Printf("%s unknown model %s — registered models: %s\n",
			paint(cRed, "✗"), paint(cWhite, name), c.listModelNames())
		return
	} else {
		c.cfg.CompressorModel = name
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	c.kernel.ReloadModels(c.cfg)
	display := c.cfg.CompressorModel
	if display == "" {
		display = "<primary model>"
	}
	fmt.Printf("%s compressor model → %s %s\n", paint(cGreen, "✓"), paint(cOrange+clrBold, display), paint(cGray, "(hot-reloaded; governs STM + project compression)"))
}

// listModelNames returns the registered model names as a comma-separated list
// for error messages.
func (c *Console) listModelNames() string {
	var names []string
	for k := range c.cfg.Models {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (c *Console) setMode(mode string) {
	switch strings.ToLower(mode) {
	case "single", "escalation", "consensus":
		c.cfg.RoutingMode = strings.ToLower(mode)
	default:
		fmt.Printf("%s invalid mode %s (single | escalation | consensus)\n", paint(cRed, "✗"), mode)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	fmt.Printf("%s routing mode → %s\n", paint(cGreen, "✓"), paint(cBlue, c.cfg.RoutingMode))
}

// setProfile switches the execution profile (parallel/sequential/auto) at
// runtime. Mirrors setMode/setSafety. The profile is hot-toggled on the
// kernel via SetExecutionProfile so it takes effect on the next Execute
// without a restart. Auto resolves per-request: sequential when only
// free-tier cloud models are registered, parallel otherwise.
// setChatMode switches the per-request chat mode (CLI ↔ GUI parity with
// the web chat's mode dropdown). general = pure conversation (no tools),
// project/auto = full tool runtime, loop = ReAct loop (needs the Agentic
// Loop master toggle on in Settings). Applied per-query via
// ApplyRequestOverrides in runQuery.
// setAlways makes a strategy verb stick until the user says otherwise.
//
// This replaced setChatMode, which had its own chat/build/loop vocabulary for
// the same question the verbs answer. Two spellings of one intent is what
// produced "(enable the Agentic Loop in /config for Loop mode to take effect)"
// — a command surface apologising for a configuration surface.
func (c *Console) setAlways(name string) {
	name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "/")))
	if name == "off" || name == "auto" || name == "none" {
		c.stickyVerb = ""
		fmt.Printf("%s strategy → chosen per message\n", paint(cGreen, "✓"))
		return
	}
	if _, ok := verb.Lookup(name); !ok {
		fmt.Printf("%s no such verb %q. Use: %s, or off\n",
			paint(cRed, "✗"), name, strings.Join(verb.Names(), ", "))
		return
	}
	c.stickyVerb = name
	fmt.Printf("%s every message → %s %s\n", paint(cGreen, "✓"), paint(cCyan, "/"+name),
		paint(cGray, "(/always off to stop)"))
}

// setBrain sets the per-session routing brain (Phase 1 parity with the GUI).
func (c *Console) setBrain(brain string) {
	brain = strings.ToLower(strings.TrimSpace(brain))
	switch brain {
	case "auto", "local", "cloud":
		c.brain = brain
	default:
		fmt.Printf("%s invalid brain %q. Use: auto, local, cloud\n", paint(cRed, "✗"), brain)
		return
	}
	fmt.Printf("%s brain → %s\n", paint(cGreen, "✓"), paint(cCyan, c.brain))
}

// setMemoryProfile sets the local model's context/RAM profile (Phase 1 parity).
func (c *Console) setMemoryProfile(profile string) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	switch profile {
	case "lean", "balanced", "max", "auto", "":
		if profile == "auto" {
			profile = ""
		}
		c.cfg.MemoryProfile = profile
	default:
		fmt.Printf("%s invalid memory profile %q. Use: lean, balanced, max, auto\n", paint(cRed, "✗"), profile)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	shown := c.cfg.MemoryProfile
	if shown == "" {
		shown = "auto"
	}
	fmt.Printf("%s memory profile → %s %s\n", paint(cGreen, "✓"), paint(cCyan, shown),
		paint(cGray, "(applies on the next local-model load)"))
}

func (c *Console) setProfile(profile string) {
	switch strings.ToLower(profile) {
	case "parallel", "sequential", "auto":
		c.cfg.ExecutionProfile = strings.ToLower(profile)
	default:
		fmt.Printf("%s invalid profile %s (parallel | sequential | auto)\n", paint(cRed, "✗"), profile)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	if c.kernel != nil {
		c.kernel.SetExecutionProfile(c.cfg.ExecutionProfile)
	}
	fmt.Printf("%s execution profile → %s\n", paint(cGreen, "✓"), paint(cCyan, c.cfg.ExecutionProfile))
}

// setLocal toggles the local LLM initialization at startup or task offloading.
func (c *Console) setLocal(args []string) {
	if len(args) == 0 {
		return
	}

	if strings.ToLower(args[0]) == "offload" {
		if len(args) < 2 {
			fmt.Printf("%s missing state for offload (on | off)\n", paint(cRed, "✗"))
			return
		}
		arg := strings.ToLower(args[1])
		switch arg {
		case "on", "true", "enable", "1":
			c.cfg.EnableLocalOffloading = true
		case "off", "false", "disable", "0":
			c.cfg.EnableLocalOffloading = false
		default:
			fmt.Printf("%s invalid state %s (on | off)\n", paint(cRed, "✗"), arg)
			return
		}
		if err := c.cfg.Save(); err != nil {
			fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
			return
		}
		state := paint(cRed, "off")
		if c.cfg.EnableLocalOffloading {
			state = paint(cGreen, "on")
		}
		fmt.Printf("%s local offload → %s\n", paint(cGreen, "✓"), state)
		return
	}

	arg := strings.ToLower(args[0])
	switch arg {
	case "force":
		// Force-local: pin routing to the local model — no cloud fallback —
		// and auto-start it now.
		c.cfg.EnableLocalLLM = true
		c.cfg.LocalMode = "force"
	case "on", "true", "enable", "1":
		c.cfg.EnableLocalLLM = true
		c.cfg.LocalMode = "on"
	case "auto":
		c.cfg.EnableLocalLLM = true
		c.cfg.LocalMode = "auto"
	case "off", "false", "disable", "0":
		c.cfg.EnableLocalLLM = false
		c.cfg.LocalMode = "off"
	default:
		fmt.Printf("%s invalid state %s (force | on | auto | off)\n", paint(cRed, "✗"), arg)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}

	// Apply the preference immediately (no restart): pin/unpin force-local
	// routing and, when local is wanted but not yet up, start the embedded
	// model on demand. In force mode a startup failure is a hard error — the
	// never-silent-fallback guarantee — so surface it and warn the user their
	// requests will fail until local is available, rather than quietly using
	// a cloud model.
	if c.kernel != nil {
		if err := c.kernel.ApplyLocalPreference(context.Background(), c.cfg); err != nil {
			fmt.Printf("%s %s\n", paint(cYellow, "⚠"), err)
		}
	}

	switch c.cfg.ResolvedLocalMode() {
	case "force":
		// ApplyLocalPreference always pins routing first, so force is active
		// here regardless of whether the model finished loading (any load
		// failure was already surfaced as the ⚠ diagnostic above).
		fmt.Printf("%s local llm → %s (%s)\n", paint(cGreen, "✓"), paint(cGreen+clrBold, "force"), paint(cGray, "cloud fallback disabled"))
	case "off":
		fmt.Printf("%s local llm auto-load → %s\n", paint(cGreen, "✓"), paint(cRed, "off"))
	default:
		fmt.Printf("%s local llm auto-load → %s (%s)\n", paint(cGreen, "✓"), paint(cGreen, "on"), c.cfg.ResolvedLocalMode())
	}
}

func (c *Console) setSafety(level string) {
	switch strings.ToLower(level) {
	case "strict", "normal", "relaxed":
		c.cfg.SafetyLevel = strings.ToLower(level)
	default:
		fmt.Printf("%s invalid level %s (strict | normal | relaxed)\n", paint(cRed, "✗"), level)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	fmt.Printf("%s safety level → %s\n", paint(cGreen, "✓"), paint(cYellow, c.cfg.SafetyLevel))
}

// printSandboxStatus reports the real, resolved sandbox mode and whether a
// backend is actually installed — replacing the old hardcoded "Active" lie.
func (c *Console) printSandboxStatus() {
	mode := c.cfg.ResolvedSandboxMode()
	sb := security.NewSandboxForMode(security.ParseMode(mode), c.cfg.SandboxWritable, nil)
	backend := string(sb.Backend)
	state := paint(cRed, "not confining")
	if sb.Available() {
		state = paint(cGreen, "confining via "+backend)
	} else if mode != "off" {
		state = paint(cYellow, "requested but no backend (install bwrap/firejail)")
	}
	fmt.Printf("%s sandbox mode=%s — %s\n", paint(cPurple, "🛡"), paint(cYellow, mode), state)
}

// setSandbox changes the shell-command sandbox mode. Takes effect on the next
// run, since the terminal tool's sandbox is wired at startup.
func (c *Console) setSandbox(mode string) {
	switch strings.ToLower(mode) {
	case "off", "auto", "on", "strict":
		c.cfg.Sandbox = strings.ToLower(mode)
	default:
		fmt.Printf("%s invalid mode %s (off | auto | on | strict)\n", paint(cRed, "✗"), mode)
		return
	}
	if err := c.cfg.Save(); err != nil {
		fmt.Printf("%s %s\n", paint(cRed, "✗"), err)
		return
	}
	fmt.Printf("%s sandbox → %s %s\n", paint(cGreen, "✓"), paint(cYellow, c.cfg.Sandbox),
		paint(cGray, "(applies on restart)"))
}
