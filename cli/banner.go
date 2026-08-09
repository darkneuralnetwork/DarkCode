package cli

// ============================================================================
// BANNER & HELP — the full-fledged startup banner and help screen for the
// interactive console. Rendered on launch and via the /help slash command.
// ============================================================================

import (
	"fmt"
	"strings"

	"github.com/darkcode/config"
	"github.com/darkcode/memory"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/tools"
)

// printBanner renders the full startup banner with architecture layers,
// runtime configuration, and a hint line.
// printBanner renders the startup header.
//
// It used to be six lines of block-capital ASCII art, a "capabilities matrix"
// of six bullets, a seven-row architecture table naming every internal layer,
// and a runtime block — around thirty lines before the first prompt.
//
// None of it was for the user. A person opening a coding agent wants to know
// it is alive, which model answers, and whether it can reach the network; the
// layer names are facts about our source tree. So this is the same information
// the GUI's status rail carries, in the same order, and everything else went.
//
// The wordmark matches the browser's: lowercase, the second half in amber.
func printBanner(cfg *config.Config, mem *memory.System, registry *tools.Registry, kernel *orchestrator.Kernel) {
	w := termWidth()
	if w > 88 {
		w = 88
	}

	modelName, modelProv := cfg.Model, cfg.Provider
	if modelName == "" {
		if id := localModelID(); id != "" {
			modelName, modelProv = id, "embedded"
		}
	}
	if modelName == "" {
		modelName, modelProv = "none registered", ""
	}

	fmt.Println()
	fmt.Printf("  %s%s\n", paint(cGrayLt+clrBold, "dark"), paint(cOrange+clrBold, "code"))

	// One line of state, mirroring the browser rail: what answers, how much
	// autonomy it has, and whether it may leave the machine.
	reach := paint(cGreen, "network")
	if cfg.AirGap {
		reach = paint(cAmber, "air-gapped")
	}
	fmt.Printf("  %s  %s  %s  %s\n",
		paint(cGray, modelHint(modelName, modelProv)),
		paint(cGray, "·"),
		paint(cGray, "safety ")+safetyLabel(parseSafetyInt(cfg.SafetyLevel)),
		reach)

	// Tools and memory are the two things whose absence changes what you can
	// ask for, so they are stated and nothing else is.
	tools, episodes := 0, 0
	if registry != nil {
		tools = len(registry.List())
	}
	if mem != nil {
		episodes = len(mem.EpisodicGet())
	}
	fmt.Printf("  %s\n", paint(cGray, fmt.Sprintf("%d tools · %d remembered runs", tools, episodes)))

	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))
	fmt.Printf("  %s %s\n\n",
		paint(cGray, "Type a request, or"),
		paint(cGrayLt, "/help")+paint(cGray, " for commands."))
}

// modelHint names the model without repeating the provider when it is already
// obvious from the model id.
func modelHint(name, provider string) string {
	if provider == "" || strings.Contains(strings.ToLower(name), strings.ToLower(provider)) {
		return name
	}
	return name + " (" + provider + ")"
}

// safetyLabel converts a SafetyLevel to a friendly label.
func safetyLabel(s int) string {
	switch orchestrator.SafetyLevel(s) {
	case orchestrator.SafetyStrict:
		return "strict"
	case orchestrator.SafetyRelaxed:
		return "relaxed"
	default:
		return "normal"
	}
}

// parseSafetyInt mirrors main.parseSafetyLevel without import cycle.
func parseSafetyInt(s string) int {
	switch strings.ToLower(s) {
	case "strict":
		return int(orchestrator.SafetyStrict)
	case "relaxed":
		return int(orchestrator.SafetyRelaxed)
	default:
		return int(orchestrator.SafetyNormal)
	}
}

// localModelID returns the loaded embedded llama.cpp model id, or "" if the
// local LLM is off / not yet running. Used by the banner's RUNTIME block so a
// local-only setup shows its model instead of a blank line.
func localModelID() string {
	p := embedded.Default()
	if p == nil {
		return ""
	}
	if p.Status().State != embedded.StateRunning {
		return ""
	}
	return p.LoadedModelID()
}
