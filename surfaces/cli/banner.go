package cli

// ============================================================================
// BANNER & HELP — the full-fledged startup banner and help screen for the
// interactive console. Rendered on launch and via the /help slash command.
// ============================================================================

import (
	"fmt"
	"strings"

	"github.com/darkcode/infra/config"
	"github.com/darkcode/infra/metrics"
	"github.com/darkcode/kernel/orchestrator"
	"github.com/darkcode/memory/memory"
	"github.com/darkcode/model/provider/embedded"
	"github.com/darkcode/tools/tools"
)

// printBanner renders the full startup banner with architecture layers,
// runtime configuration, and a hint line.
func printBanner(cfg *config.Config, mem *memory.System, registry *tools.Registry, kernel *orchestrator.Kernel) {
	w := termWidth()
	if w > 100 {
		w = 100
	}

	ascii := ` ██████╗  █████╗ ██████╗ ██╗  ██╗ ██████╗ ██████╗ ██████╗ ███████╗
 ██╔══██╗██╔══██╗██╔══██╗██║ ██╔╝██╔════╝██╔═══██╗██╔══██╗██╔════╝
 ██║  ██║███████║██████╔╝█████╔╝ ██║     ██║   ██║██║  ██║█████╗
 ██║  ██║██╔══██║██╔══██╗██╔═██╗ ██║     ██║   ██║██║  ██║██╔══╝
 ██████╔╝██║  ██║██║  ██║██║  ██╗╚██████╗╚██████╔╝██████╔╝███████╗
 ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝ ╚═════╝ ╚═════╝ ╚══════╝`

	// Render the ASCII logo line-by-line in orange.
	for _, line := range strings.Split(ascii, "\n") {
		fmt.Println(paint(cOrange, line))
	}
	fmt.Println()

	subtitle := bold("  Enterprise AI Developer Platform") + paint(cGray, "  ·  written in Go  ·  ") + paint(cAmber, "v1.0.0")
	fmt.Println(subtitle)
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	// Capabilities Matrix
	fmt.Println(paint(cGreen+clrBold, "  CAPABILITIES MATRIX"))
	fmt.Printf("   %s %s   %s %s   %s %s\n",
		paint(cBlue, "●"), paint(cWhite, "Multi-Model Consensus"),
		paint(cBlue, "●"), paint(cWhite, "Auto-Healing Loop"),
		paint(cBlue, "●"), paint(cWhite, "Security Sandbox"),
	)
	fmt.Printf("   %s %s   %s %s   %s %s\n",
		paint(cBlue, "●"), paint(cWhite, "gRPC Plugin Engine   "),
		paint(cBlue, "●"), paint(cWhite, "Observability UI "),
		paint(cBlue, "●"), paint(cWhite, "6-Layer Memory  "),
	)
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	// Architecture layers
	layers := []struct {
		id   string
		name string
		desc string
	}{
		{"L1", "Orchestration Kernel", "planning · delegating · self-healing loop"},
		{"L2", "Verification Pipeline", "syntax · linting · compiler · tests"},
		{"L3", "Model Router", "single · escalation · consensus"},
		{"L4", "Memory System", "STM · episodic · semantic · procedural"},
		{"L5", "Security Sandbox", "firejail · namespace isolation"},
		{"L6", "Tool Runtime", "terminal · file · plugins · search · web"},
		{"L7", "Observability", "live telemetry · pprof · traces"},
	}
	fmt.Println(paint(cAmber+clrBold, "  ARCHITECTURE"))
	for _, l := range layers {
		fmt.Printf("   %s  %s  %s\n",
			paint(cOrange+clrBold, l.id),
			paint(cWhite, padRight(l.name, 26)),
			paint(cGray, l.desc))
	}
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	// Runtime configuration
	fmt.Println(paint(cAmber+clrBold, "  RUNTIME"))
	safety := safetyLabel(parseSafetyInt(cfg.SafetyLevel))

	// Primary model line. In a local-only setup (cfg.Model == ""), fall back
	// to the embedded llama.cpp model so the banner never shows a blank model.
	modelName := cfg.Model
	modelProv := cfg.Provider
	if modelName == "" {
		if id := localModelID(); id != "" {
			modelName = id
			modelProv = "embedded"
		}
	}
	fmt.Printf("   %s  %s  %s\n",
		paint(cGray, "Model"),
		paint(cWhite+clrBold, modelName),
		paint(cGray, "("+modelProv+")"))

	// Local LLM line — shown when the embedded llama.cpp server is enabled,
	// regardless of whether a cloud primary is also configured.
	if cfg.LocalEnabled() {
		localLine := "disabled"
		if id := localModelID(); id != "" {
			localLine = id + " (llama.cpp · running)"
		} else if st := embedded.Default(); st != nil && st.Status().State == embedded.StateStarting {
			localLine = "starting…"
		} else {
			localLine = "enabled (not loaded)"
		}
		fmt.Printf("   %s  %s\n",
			paint(cGray, "Local"),
			paint(cGreen, localLine))
	}

	fmt.Printf("   %s  %s  %s  %s  %s\n",
		paint(cGray, "Routing"),
		paint(cBlue, cfg.RoutingMode),
		paint(cGray, "· Safety"),
		paint(cYellow, safety),
		paint(cGray, "· Concurrency "+fmtNum(cfg.MaxConcurrent)))

	toolCount := 0
	if registry != nil {
		toolCount = len(registry.List())
	}
	memStats := "4-tier"
	if mem != nil {
		memStats = mem.ShortSummary()
	}
	fmt.Printf("   %s  %s  %s  %s  %s\n",
		paint(cGray, "Tools"),
		paint(cGreen, fmtNum(toolCount)+" registered"),
		paint(cGray, "· Memory"),
		paint(cPurple, memStats),
		paint(cGray, "· Providers "+fmtNum(len(config.Providers()))))

	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	// Metrics hint
	snap := metrics.Default.Snapshot()
	if snap.TotalRequests > 0 {
		fmt.Printf("   %s  %s tokens · %s · %s requests  %s\n",
			paint(cAmber+clrBold, "USAGE"),
			paint(cOrange, fmtNum(snap.TotalTokens)),
			paint(cGreen, fmtCost(snap.TotalCost)),
			paint(cBlue, fmtNum(snap.TotalRequests)),
			paint(cGray, "(since "+fmtTimeShort(snap.Since)+")"))
		fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))
	}

	fmt.Println()
	fmt.Printf("  %s  %s\n",
		paint(cGreen+clrBold, "►"),
		bold("Type a message to begin"))
	fmt.Printf("     %s   %s\n",
		paint(cGray, "/help"),
		paint(cGray, "for commands  ·  /monitor for live dashboard  ·  /quit to exit"))
	fmt.Println()
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
