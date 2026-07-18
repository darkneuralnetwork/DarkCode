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
	"github.com/darkcode/metrics"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/tools"
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
	if cfg.EnableLocalLLM {
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

// printHelpScreen renders the detailed help screen.
func printHelpScreen() {
	w := termWidth()
	if w > 96 {
		w = 96
	}

	fmt.Println()
	fmt.Println(paint(cOrange+clrBold, "  DARKCODE · COMMAND REFERENCE"))
	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))

	helpSection := func(title string) {
		fmt.Printf("  %s\n", paint(cAmber+clrBold, title))
	}
	helpRow := func(cmd, desc string) {
		fmt.Printf("    %s   %s\n", padRight(paint(cWhite, cmd), 42), paint(cGray, desc))
	}

	helpSection("CHAT")
	helpRow("<message>", "Send a message to the orchestrator (paste multi-line text — the whole block is sent as one message)")
	helpRow("Ctrl+C", "Interrupt the current request")
	helpRow("Ctrl+D", "Exit the console")
	fmt.Println()

	helpSection("SESSION & MEMORY")
	helpRow("/new, /reset", "Start a fresh session (clears short-term memory & permissions)")
	helpRow("/memory", "Show the 4-tier memory summary")
	helpRow("/skills", "List procedural memory (learned skills)")
	helpRow("/episodes", "Show recent episodic memory entries")
	helpRow("/know", "Knowledge graph nodes, edges & top concept relations")
	helpRow("/learning", "Show learning engine stats and strategies")
	helpRow("/projects", "Manage long-lived project contexts (/projects for help)")
	helpRow("/plan", "View the current Implementation Plan for the active project")
	helpRow("/workflow", "View the current Workflow Architecture for the active project")
	fmt.Println()

	helpSection("MODELS & PROVIDERS")
	helpRow("/models", "List registered models")
	helpRow("/models add", "Register a model interactively (or via args: <provider> <model> [api_key])")
	helpRow("/models remove", "Remove a model: /models remove <model>")
	helpRow("/models primary", "Set primary: /models primary <model>")
	helpRow("/models test", "Test connectivity: /models test [model] (default: primary)")
	helpRow("/models disable", "Temporarily disable: /models disable <model> [duration] (default 1h)")
	helpRow("/models enable", "Reverse a disable early: /models enable <model>")
	helpRow("/providers", "Browse the 19-provider catalogue")
	helpRow("/providers <id>", "Show models for a provider (e.g. /providers openai)")
	helpRow("/model <name>", "Switch the active model on the fly (hot-reload)")
	fmt.Println()

	helpSection("ROUTING & SAFETY")
	helpRow("/mode <single|escalation|consensus>", "Change routing mode")
	helpRow("/chatmode <smart|general|project|loop>", "Per-request chat mode (CLI ↔ GUI parity): smart=auto intent, general=no tools, project=full tools, loop=ReAct")
	helpRow("/profile <parallel|sequential|auto>", "Change execution profile (parallelism switcher)")
	helpRow("/safety <strict|normal|relaxed>", "Change safety level (approval policy)")
	helpRow("/local <on|off> | offload <on|off>", "Toggle Local LLM auto-initialization or task offloading")
	helpRow("/compressor [model|primary]", "Select the context-compression model (governs STM + project summary). Use 'primary' to clear.")
	helpRow("/permissions", "Show permission gate stats · /permissions reset clears session")
	helpRow("/config", "Show current configuration")
	fmt.Println()

	helpSection("APPROVAL & LOGS")
	helpRow("(auto)", "Dangerous actions prompt: [1] once  [2] session  [3] deny")
	helpRow("/log", "Show the full hidden trace: events + before→after file diffs")
	helpRow("/events", "Toggle the minimal progress indicator")
	helpRow("/history", "Show the command history")
	helpRow("/audit", "Show recent audit log entries")
	fmt.Println()

	helpSection("OBSERVABILITY & ENTERPRISE")
	helpRow("/monitor", "Open the full-screen live monitoring dashboard")
	helpRow("/stats", "Show detected hardware capabilities (CPU / RAM / GPU / tier)")
	helpRow("/sandbox", "Show the security sandbox status and metrics")
	helpRow("/pipeline", "Show the deterministic verification pipeline stages")
	helpRow("/plugins", "Show the enterprise plugin system status")
	helpRow("/usage", "Print a compact usage summary (tokens · cost · latency)")
	helpRow("/status", "Show orchestrator kernel status")
	helpRow("/tools", "List registered tools (built-in + sources)")
	helpRow("/tools sources", "List tool sources and their connect state")
	helpRow("/tools connect mcp <name> <cmd> [args]", "Connect a stdio MCP server at runtime")
	helpRow("/tools connect mcp-http <name> <url>", "Connect an HTTP MCP server at runtime")
	helpRow("/tools connect file <name> <path>", "Load in-house ITF tools from a file/dir")
	helpRow("/tools disconnect <id>", "Disconnect a source (keeps its definition)")
	helpRow("/tools remove <id>", "Disconnect + delete a source")
	fmt.Println()

	helpSection("SYSTEM")
	helpRow("/gui", "Switch to the web GUI (resumes execution in the browser)")
	helpRow("/help, /?", "Show this help")
	helpRow("/quit, /exit", "Exit the console")
	fmt.Println()

	fmt.Println(paint(cGray, "  "+strings.Repeat("─", w-4)))
	fmt.Println(paint(cGray, "  Stdout stays clean: only file changes (before → after) and the"))
	fmt.Println(paint(cGray, "  final answer are printed. All intermediate events are captured in"))
	fmt.Println(paint(cGray, "  /log. Dangerous tool calls require explicit approval."))
	fmt.Println()
}
