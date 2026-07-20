package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func (c *Console) printConfig() {
	fmt.Println(paint(cAmber+clrBold, "CONFIGURATION"))
	fmt.Printf("  %-16s %s\n", paint(cGray, "model"), paint(cOrange+clrBold, c.cfg.Model))
	fmt.Printf("  %-16s %s\n", paint(cGray, "provider"), paint(cWhite, c.cfg.Provider))
	fmt.Printf("  %-16s %s\n", paint(cGray, "base_url"), paint(cGray, c.cfg.BaseURL))
	fmt.Printf("  %-16s %s\n", paint(cGray, "routing_mode"), paint(cBlue, c.cfg.RoutingMode))
	prof := c.cfg.ExecutionProfile
	if prof == "" {
		prof = "auto"
	}
	fmt.Printf("  %-16s %s\n", paint(cGray, "execution_profile"), paint(cCyan, prof))
	fmt.Printf("  %-16s %s\n", paint(cGray, "safety_level"), paint(cYellow, c.cfg.SafetyLevel))
	fmt.Printf("  %-16s %s\n", paint(cGray, "max_turns"), paint(cWhite, fmtNum(c.cfg.MaxTurns)))
	fmt.Printf("  %-16s %s\n", paint(cGray, "max_concurrent"), paint(cWhite, fmtNum(c.cfg.MaxConcurrent)))
	fmt.Printf("  %-16s %s\n", paint(cGray, "compress_context"), paint(cWhite, fmt.Sprintf("%v", c.cfg.CompressContext)))
	cm := c.cfg.CompressorModel
	if cm == "" {
		cm = "<primary>"
	}
	fmt.Printf("  %-16s %s\n", paint(cGray, "compressor_model"), paint(cWhite, cm))
	fmt.Printf("  %-16s %s\n", paint(cGray, "memory_dir"), paint(cGray, c.cfg.MemoryDir))
	if len(c.cfg.Models) > 0 {
		fmt.Printf("  %-16s\n", paint(cGray, "registered models:"))
		for k, m := range c.cfg.Models {
			primary := ""
			if k == c.cfg.Model {
				primary = paint(cOrange, " (primary)")
			}
			fmt.Printf("     • %s  %s  %s%s\n", paint(cWhite, m.Model), paint(cGray, m.Provider), paint(cGray, m.BaseURL), primary)
		}
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
func (c *Console) setChatMode(mode string) {
	mode = strings.ToLower(mode)
	// Accept the GUI's Chat/Build vocabulary plus back-compat aliases.
	switch mode {
	case "chat", "general":
		c.chatMode = "chat"
	case "build", "project", "smart":
		c.chatMode = "build"
	case "loop":
		c.chatMode = "loop"
	default:
		fmt.Printf("%s invalid chat mode %q. Use: chat, build, loop\n", paint(cRed, "✗"), mode)
		return
	}
	note := ""
	if c.chatMode == "loop" && !c.cfg.AgenticLoop {
		note = paint(cYellow, "  (enable the Agentic Loop in /config for Loop mode to take effect)")
	}
	fmt.Printf("%s chat mode → %s%s\n", paint(cGreen, "✓"), paint(cCyan, c.chatMode), note)
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
