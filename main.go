package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/darkcode/config"
	"github.com/darkcode/llm"
	"github.com/darkcode/cli/tui"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
)

func main() {
	var (
		query       string
		model       string
		toolsFlag   bool
		statusFlag  bool
		help        bool
		uiMode      bool
		tuiFlag     bool
		guiFlag     bool
		portFlag    string
		routingMode string
		safetyLevel string
		addModel    string
		addProvider string
		addKey      string
		removeModel string
		debugFlag   bool
	)

	flag.StringVar(&query, "q", "", "Single query (non-interactive mode)")
	flag.StringVar(&query, "query", "", "Single query (non-interactive mode)")
	flag.StringVar(&model, "m", "", "Override model")
	flag.StringVar(&model, "model", "", "Override model")
	flag.BoolVar(&toolsFlag, "tools", false, "List registered tools and exit")
	flag.BoolVar(&statusFlag, "status", false, "Show orchestrator system status and exit")
	flag.BoolVar(&help, "help", false, "Show help")
	flag.BoolVar(&uiMode, "ui", false, "Enable UI event streaming mode")
	flag.BoolVar(&tuiFlag, "tui", false, "Start new BubbleTea TUI mode")
	flag.BoolVar(&guiFlag, "gui", false, "Start GUI mode: web UI + API on http://localhost:12345")
	flag.StringVar(&portFlag, "port", "", "Port for HTTP server (default: 12345 for --gui)")
	flag.StringVar(&routingMode, "mode", "", "Routing mode: single, escalation, consensus")
	flag.StringVar(&safetyLevel, "safety", "", "Safety level: strict, normal, relaxed")
	flag.StringVar(&addModel, "add-model", "", "Register a model and exit: --add-model <model> --provider <id> --api-key <key>")
	flag.StringVar(&addProvider, "provider", "", "Provider id for --add-model (e.g. openai, groq, ollama)")
	flag.StringVar(&addKey, "api-key", "", "API key for --add-model")
	flag.StringVar(&removeModel, "remove-model", "", "Remove a registered model and exit: --remove-model <model>")
	listProvidersFlag := flag.Bool("providers", false, "List the LLM provider catalogue and exit")
	listModelsFlag := flag.Bool("models", false, "List registered models and exit")
	flag.BoolVar(&debugFlag, "debug", false, "Enable /debug/pprof/* profiler endpoints on the GUI server (off by default)")
	flag.Parse()

	// GUI mode: the web UI + API is always bound to localhost (127.0.0.1).
	// There is no --serve / network-exposure mode; the only web entry point is
	// --gui (or the /gui command typed in the CLI console).
	if guiFlag && portFlag == "" {
		portFlag = "12345"
	}
	bindAddr := ""
	if guiFlag {
		bindAddr = "127.0.0.1:" + portFlag
	}

	if help {
		printHelp()
		os.Exit(0)
	}

	// Handle --tools flag early — no API key needed
	if toolsFlag {
		listToolsStandalone()
		os.Exit(0)
	}

	if tuiFlag {
		if err := tui.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Apply CLI overrides
	if model != "" {
		cfg.Model = model
	}
	if routingMode != "" {
		cfg.RoutingMode = routingMode
	}
	if safetyLevel != "" {
		cfg.SafetyLevel = safetyLevel
	}
	if uiMode {
		cfg.UIMode = true
	}
	cfg.DebugPprof = debugFlag

	// --add-model: register a model non-interactively, save, and exit.
	// This makes the CLI a first-class way to manage models alongside the
	// GUI and direct .config editing.
	if addModel != "" {
		addModelCLI(cfg, addModel, addProvider, addKey)
		return
	}
	if removeModel != "" {
		removeModelCLI(cfg, removeModel)
		return
	}
	if *listProvidersFlag {
		listProvidersStandalone()
		return
	}
	if *listModelsFlag {
		listModelsStandalone(cfg)
		return
	}

	// Validate config - warn if API key missing but allow tool to start
	if err := cfg.Validate(); err != nil {
		if !uiMode && !guiFlag && query == "" {
			// Interactive CLI mode and API key is missing. Run wizard!
			config.RunInteractiveSetup(cfg)
		} else {
			p := portFlag
			if p == "" {
				p = "12345" // Default GUI port
			}
			fmt.Fprintf(os.Stderr, "\033[38;5;214m\033[1m⚠ Configuration Warning:\033[0m %v\n", err)
			fmt.Fprintf(os.Stderr, "  Please configure your API key via the GUI (http://localhost:%s/config)\n", p)
			fmt.Fprintf(os.Stderr, "  or in the CLI using '/models add'.\n\n")
		}
	}

	// === ORCHESTRATOR MODE ===
	runOrchestrator(cfg, query, statusFlag, portFlag, guiFlag, bindAddr)
}

// runOrchestrator wires the full 6-layer system and runs the query.
func runOrchestrator(cfg *config.Config, query string, statusOnly bool, portFlag string, guiFlag bool, bindAddr string) {
	runner := NewAppRunner(cfg, query, statusOnly, portFlag, guiFlag, bindAddr)
	runner.WireUp()
	runner.Execute()
}


// === Helpers ===

func parseSafetyLevel(s string) orchestrator.SafetyLevel {
	switch strings.ToLower(s) {
	case "strict":
		return orchestrator.SafetyStrict
	case "relaxed":
		return orchestrator.SafetyRelaxed
	default:
		return orchestrator.SafetyNormal
	}
}

func getFastModel(rtr *router.Router, cfg *config.Config) (*llm.Client, string) {
	// Look for a model whose tier is "fast" in the models map.
	for _, mc := range cfg.Models {
		if strings.EqualFold(mc.Tier, "fast") {
			return llm.NewClient(mc.BaseURL, mc.APIKey, mc.Model), mc.Model
		}
	}
	return llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model), cfg.Model
}

// addModelCLI registers a model non-interactively via flags, saves config,
// and prints the result. Mirrors exactly what the GUI and /models add do.
func addModelCLI(cfg *config.Config, modelID, providerID, apiKey string) {
	if providerID == "" {
		// Try to infer from the model id (e.g. "openai/gpt-4o" → openai)
		if idx := strings.Index(modelID, "/"); idx > 0 {
			providerID = modelID[:idx]
		} else {
			providerID = "openrouter"
		}
	}
	p, ok := config.LookupProvider(providerID)
	if !ok {
		fmt.Fprintf(os.Stderr, "✗ unknown provider: %s\n  run 'darkcode --providers' to see the catalogue\n", providerID)
		os.Exit(1)
	}
	baseURL := p.BaseURL
	if apiKey == "" && p.AuthScheme == config.AuthNone {
		apiKey = "local"
	}
	if cfg.Models == nil {
		cfg.Models = make(map[string]config.ModelConfig)
	}
	tier := config.ResolveTier(providerID, modelID)
	cfg.Models[modelID] = config.ModelConfig{
		Model:    modelID,
		Provider: providerID,
		APIKey:   apiKey,
		BaseURL:  baseURL,
		Tier:     tier,
	}
	// If no primary model is set, make this the primary.
	if cfg.Model == "" || cfg.APIKey == "" {
		cfg.Model = modelID
		cfg.Provider = providerID
		cfg.BaseURL = baseURL
		cfg.APIKey = apiKey
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to save config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ registered %s  [%s]  (%s @ %s)\n", modelID, tier, providerID, baseURL)
	if apiKey != "" {
		fmt.Printf("  api key: set\n")
	} else {
		fmt.Printf("  api key: (none — local provider)\n")
	}
	fmt.Printf("  config saved to %s\n", config.ConfigPath())
}

// removeModelCLI removes a model from config non-interactively.
func removeModelCLI(cfg *config.Config, modelID string) {
	if _, ok := cfg.Models[modelID]; !ok {
		fmt.Fprintf(os.Stderr, "✗ no such model: %s\n", modelID)
		os.Exit(1)
	}
	delete(cfg.Models, modelID)
	if cfg.Model == modelID {
		for _, mc := range cfg.Models {
			cfg.Model = mc.Model
			cfg.Provider = mc.Provider
			cfg.BaseURL = mc.BaseURL
			cfg.APIKey = mc.APIKey
			break
		}
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to save config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ removed %s\n  config saved to %s\n", modelID, config.ConfigPath())
}

// listProvidersStandalone prints the full provider catalogue to stdout.
func listProvidersStandalone() {
	provs := config.Providers()
	fmt.Printf("LLM Provider Catalogue (%d providers):\n\n", len(provs))
	for _, p := range provs {
		auth := p.AuthScheme
		if p.Local {
			auth = "local"
		}
		fmt.Printf("  %-14s %-26s [%s] %d models\n", p.ID, p.Name, auth, len(p.Models))
		fmt.Printf("    base: %s\n", p.BaseURL)
		if p.KeyURL != "" {
			fmt.Printf("    keys: %s\n", p.KeyURL)
		}
		for _, m := range p.Models {
			ctxK := fmt.Sprintf("%dk", m.ContextWindow/1000)
			if m.ContextWindow >= 1000000 {
				ctxK = fmt.Sprintf("%.1fM", float64(m.ContextWindow)/1e6)
			}
			price := "free"
			if m.InputPrice > 0 || m.OutputPrice > 0 {
				price = fmt.Sprintf("$%.2f/$%.2f per 1M", m.InputPrice, m.OutputPrice)
			}
			fmt.Printf("    • %-34s [%s] %s ctx  %s\n", m.ID, m.Tier, ctxK, price)
		}
		fmt.Println()
	}
	fmt.Println("Add a model with:")
	fmt.Println("  darkcode --add-model <model-id> --provider <provider-id> --api-key <key>")
	fmt.Println("  e.g. darkcode --add-model gpt-4o --provider openai --api-key sk-...")
	fmt.Println("  or interactively: darkcode  →  /models add")
}

// listModelsStandalone prints the currently registered models.
func listModelsStandalone(cfg *config.Config) {
	fmt.Printf("Registered Models (%d):\n\n", len(cfg.Models)+1)
	tier := config.ResolveTier(cfg.Provider, cfg.Model)
	fmt.Printf("  ★ %-30s [%s] %s  (primary)\n", cfg.Model, tier, cfg.Provider)
	for _, m := range cfg.Models {
		t := m.Tier
		if t == "" {
			t = config.ResolveTier(m.Provider, m.Model)
		}
		fmt.Printf("  • %-30s [%s] %s\n", m.Model, t, m.Provider)
	}
}

func listToolsStandalone() {
	registry := tools.NewRegistry()
	tools.RegisterBuiltinTools(registry, nil, nil)
	tools.RegisterMemoryTool(registry, tools.NewSemanticMemoryTool(nil, nil))
	entries := registry.List()
	fmt.Printf("Registered Tools (%d):\n", len(entries))
	for _, entry := range entries {
		desc := entry.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		fmt.Printf("  - %-15s [%s]: %s\n", entry.Name, entry.Category, desc)
	}
}

func printHelp() {
	fmt.Println(`DarkCode - The next-generation autonomous AI engineering platform.

Usage:
  darkcode [flags]

Flags:
  -q, --query TEXT       Run a single query (non-interactive)
  -m, --model NAME       Override the model
  --mode MODE            Routing mode: single, escalation, consensus
  --safety LEVEL         Safety level: strict, normal, relaxed
  --ui                   Enable UI event streaming (observable execution)
  --gui                  Start GUI mode: web UI + API on http://localhost:12345
                         (always bound to 127.0.0.1; also reachable via /gui in the CLI)
  --port PORT            Port for the GUI server (default: 12345)
  --status               Show system status and exit
  --tools                List registered tools and exit
  --providers            List the LLM provider catalogue and exit
  --models               List registered models and exit
  --add-model MODEL      Register a model and exit (use with --provider, --api-key)
  --remove-model MODEL   Remove a registered model and exit
  --help                 Show this help

Architecture (6 Layers):
  1. Orchestration Kernel  - planning, delegating, verifying
  2. Model Router          - multi-model: single/escalation/consensus
  3. Compression Agent     - context reduction between steps
  4. Memory System         - STM, episodic, semantic, procedural
  5. Sub-Agent System      - executive, planner, worker, critic, UI
  6. Tool Runtime          - terminal, file, search, web, memory

Environment Variables:
  DARKCODE_API_KEY       API key for the LLM provider
  DARKCODE_BASE_URL      Base URL for the LLM API
  DARKCODE_MODEL         Model name
  OPENROUTER_API_KEY      OpenRouter API key (auto-detected)
  OPENAI_API_KEY          OpenAI API key (auto-detected)
  DEEPSEEK_API_KEY        DeepSeek API key (auto-detected)
  ANTHROPIC_API_KEY       Anthropic API key (auto-detected)

Slash Commands (in interactive mode):
  /status                 Show orchestrator system status
  /memory                 Show memory summary
  /tools                  List tools (built-in + sources)
  /tools sources          List MCP / in-house tool sources
  /tools connect mcp <name> <cmd> [args]   Connect an MCP server (stdio)
  /tools connect mcp-http <name> <url>      Connect an MCP server (HTTP)
  /tools connect file <name> <path>         Load in-house ITF tools
  /tools disconnect <id>  Disconnect a tool source
  /tools remove <id>      Remove a tool source
  /skills                 List procedural memory (skills)
  /episodes               List recent episodic memory
  /config                 Show configuration
  /log                    Show hidden trace + before->after file diffs
  /permissions            Show approval stats (or /permissions reset)
  /new, /reset            Start new session (clear STM)
  /help                   Show help
  /quit                   Exit`)
}
