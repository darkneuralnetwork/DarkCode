package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds all agent configuration.
type Config struct {
	// --- Single model (backward compatible) ---
	Model         string  `json:"model"`
	Provider      string  `json:"provider"`
	BaseURL       string  `json:"base_url"`
	APIKey        string  `json:"api_key"`
	MaxTurns      int     `json:"max_turns"`
	Temperature   float64 `json:"temperature"`
	ContextLength int     `json:"context_length"`
	SystemPrompt  string  `json:"system_prompt"`

	// --- Multi-model (Layer 2: Model Router) ---
	Models      map[string]ModelConfig `json:"models,omitempty"`
	RoutingMode string                 `json:"routing_mode,omitempty"` // single, escalation, consensus

	// --- Orchestrator settings ---
	UIMode          bool   `json:"ui_mode,omitempty"`
	SafetyLevel     string `json:"safety_level,omitempty"` // strict, normal, relaxed
	MaxConcurrent   int    `json:"max_concurrent,omitempty"`
	CompressContext bool   `json:"compress_context,omitempty"`

	// UseCtxEngine enables the intelligent context-assembly engine
	// (dedup + TF-IDF ranking + budget trimming) for the General-mode
	// fast path. Default false (raw STM append) to preserve behavior.
	UseCtxEngine bool `json:"use_ctx_engine,omitempty"`

	// --- Execution Profile (parallelism switcher) ---
	// Controls how the two parallelism points (DAG sub-agent executor +
	// consensus fan-out) run: "parallel" (today's behavior), "sequential"
	// (serial — safe on free-tier models with strict RPM limits), or "auto"
	// (default — resolves to sequential when only free-tier cloud models are
	// registered, parallel otherwise). Retry/backoff is always on regardless
	// of profile. Hot-toggled from the Settings tab.
	ExecutionProfile string `json:"execution_profile,omitempty"`

	// --- Context Compressor ---
	// The model used for context compression (Layer 3). If empty, the primary
	// model is used. The user can pick any registered model from the GUI so a
	// cheaper/faster model handles compression while the primary handles reasoning.
	CompressorModel string `json:"compressor_model,omitempty"`

	// --- Agentic Loop (looping technology) ---
	// When true the kernel runs an explicit Sense-Think-Act (ReAct) loop
	// instead of the single-pass DAG decomposition. Optional — toggled from
	// the Settings tab; the header shows the current on/off state.
	AgenticLoop bool `json:"agentic_loop,omitempty"`
	MaxLoops    int  `json:"max_loops,omitempty"`

	// --- Memory ---
	MemoryDir string `json:"memory_dir,omitempty"`

	// --- Projects ---
	// Long-lived project context (per-project folders on disk).
	ProjectsDir string `json:"projects_dir,omitempty"`
	
	// --- Local LLM ---
	// Toggle whether to automatically load the local llama.cpp engine at startup.
	EnableLocalLLM bool `json:"enable_local_llm"`
	// Toggle whether to offload simple tasks (explain error, code review) to local LLM.
	EnableLocalOffloading bool `json:"enable_local_offloading"`
	// LocalModelRole is the consensus role assigned to the local/embedded model
	// (critic, skeptic, knowledge_booster, …). Empty = no explicit role (the
	// model stays at its size-tier: medium_local/tiny_local). Unlike cloud
	// models (whose role lives in ModelConfig.Role), the local model is a
	// runtime entity not stored in the Models map, so its role needs its own
	// field to survive restarts.
	LocalModelRole string `json:"local_model_role,omitempty"`

	// EmbeddedContextSize overrides the llama-server context window (-c) for
	// the local model. 0 = auto (RAM-aware default from computeLaunchOpts:
	// ≥ 32768 on systems with ≥ 4GB RAM). >0 = always use this value, winning
	// over the RAM guard. Useful for forcing a larger/smaller context than the
	// auto-detected default.
	EmbeddedContextSize int `json:"embedded_context_size,omitempty"`

	// EmbeddedIdleTimeoutMinutes unloads the local model after this many
	// minutes of inactivity, freeing RAM/VRAM. 0 (default) = disabled — the
	// model stays resident once loaded. Opt-in because an unexpected unload
	// means the next request pays a full reload; users who want the RAM back
	// should set this explicitly.
	EmbeddedIdleTimeoutMinutes int `json:"embedded_idle_timeout_minutes,omitempty"`

	// --- Tool Sources ---
	// External MCP servers and in-house (Internal Tool Format) tool files
	// that are registered with the tool registry and can be connected /
	// disconnected at runtime from both the CLI and the GUI.
	ToolSources []ToolSourceConfig `json:"tool_sources,omitempty"`

	// DebugPprof enables the /debug/pprof/* profiler endpoints on the GUI
	// server. Off by default — pprof leaks process args/env and lets any
	// caller trigger CPU-consuming profile captures, so it must be opted
	// into explicitly (--debug) rather than always registered.
	DebugPprof bool `json:"-"`
}

// ModelConfig defines a single model in a multi-model setup.
// The map key in Config.Models is the model name; the tier is stored here
// so that CLI, GUI, and direct .config editing all produce identical results.
type ModelConfig struct {
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
	Tier     string `json:"tier,omitempty"` // reasoning | coding | fast | local | critic
	Role     string `json:"role,omitempty"` // consensus role: critic | skeptic | knowledge_booster | creative | analyst | verifier | general
}

// ToolSourceConfig is the persistable definition of a tool source. It is the
// .config representation of a tools.SourceConfig (kept as a plain struct here
// to avoid a config → tools import cycle). The Type field selects the
// transport: "mcp_stdio" (local MCP server process), "mcp_http" (remote MCP
// server), or "internal" (an in-house Internal Tool Format file/dir).
type ToolSourceConfig struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`              // mcp_stdio | mcp_http | internal
	Command     string            `json:"command,omitempty"` // mcp_stdio: executable
	Args        []string          `json:"args,omitempty"`    // mcp_stdio: args
	Env         map[string]string `json:"env,omitempty"`     // mcp_stdio: env overrides
	URL         string            `json:"url,omitempty"`     // mcp_http: endpoint
	Headers     map[string]string `json:"headers,omitempty"` // mcp_http: extra headers
	Path        string            `json:"path,omitempty"`    // internal: ITF file or dir
	AutoConnect bool              `json:"auto_connect,omitempty"`
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() *Config {
	return &Config{
		Model:            "",
		Provider:         "embedded",
		BaseURL:          "http://127.0.0.1:0/v1",
		APIKey:           "",
		MaxTurns:         50,
		Temperature:      0.7,
		ContextLength:    16000,
		SystemPrompt:     DefaultSystemPrompt,
		RoutingMode:      "single",
		SafetyLevel:      "normal",
		MaxConcurrent:    3,
		CompressContext:  true,
		ExecutionProfile: "auto",
		AgenticLoop:      false,
		MaxLoops:         3,
		MemoryDir:        ".darkcode/memory",
		ProjectsDir:           ".darkcode/projects",
		EnableLocalLLM:        false,
		EnableLocalOffloading: false,
	}
}

const DefaultSystemPrompt = `You are DarkCode, a modular AI agent operating system built in Go.

You are NOT a chatbot. You are a distributed intelligence system with:
- Layer 1: Orchestration Kernel (planning, delegating, verifying)
- Layer 2: Multi-Model Router (single, escalation, consensus modes)
- Layer 3: Compression Agent (context reduction between steps)
- Layer 4: Memory System (short-term, episodic, semantic, procedural)
- Layer 5: Sub-Agent System (executive, planner, worker, critic, compression, UI)
- Layer 6: Tool Runtime (terminal, file, search, web, memory)

EXECUTION LOOP: Observe -> Compress -> Plan -> Route -> Execute -> Validate -> Merge -> Store

You use tools to take real action. When you get multiple tool calls, they execute
concurrently via goroutines. Keep working until the task is complete.

SAFETY: Destructive actions require approval. All tool outputs are logged.

SELF-IMPROVEMENT: After successful tasks, reusable patterns are extracted as skills.

When you encounter errors, report them honestly and try alternatives.`

// ConfigPath returns the path to the config file.
//
// The config lives in the current working directory as ".config" so that
// configuration is project-specific and easily tracked. This matches the
// GUI (Settings tab) and CLI ("/models add", "/config set") which both read
// and write the same "./.config" file, as documented in the README. The
// legacy ~/.darkcode/config.json location is only used as a fallback when
// the current working directory cannot be determined.
func ConfigPath() string {
	cwd, err := os.Getwd()
	if err == nil {
		return filepath.Join(cwd, ".config")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".darkcode", "config.json")
}

// Load reads the config from disk, falling back to defaults.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Try environment variables as fallback
			applyEnv(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Environment variables override config file
	applyEnv(cfg)

	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = 50
	}
	if cfg.ContextLength == 0 {
		cfg.ContextLength = 16000
	}
	if cfg.RoutingMode == "" {
		cfg.RoutingMode = "single"
	}
	if cfg.SafetyLevel == "" {
		cfg.SafetyLevel = "normal"
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 3
	}
	if cfg.ExecutionProfile == "" {
		cfg.ExecutionProfile = "auto"
	}
	if cfg.MemoryDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cfg.MemoryDir = filepath.Join(home, ".darkcode", "memory")
		} else {
			cwd, _ := os.Getwd()
			cfg.MemoryDir = filepath.Join(cwd, ".darkcode", "memory")
		}
	}
	if cfg.ProjectsDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cfg.ProjectsDir = filepath.Join(home, ".darkcode", "projects")
		} else {
			cwd, _ := os.Getwd()
			cfg.ProjectsDir = filepath.Join(cwd, ".darkcode", "projects")
		}
	}

	// Warn-only validation: never abort boot (preserves existing behavior),
	// but surface portability/logic problems to stderr so they aren't silent.
	if verr := cfg.Validate(); verr != nil {
		fmt.Fprintf(os.Stderr, "Warning: config validation: %v\n", verr)
	}

	return cfg, nil
}

// Save writes the config to disk.
//
// The file is written with mode 0600 (owner-read/write only) because it holds
// the API key (and per-model keys). It was previously 0644, which left live
// keys readable by any user on the host. Existing files on disk keep their
// old mode; only newly-written saves are tightened.
func (cfg *Config) Save() error {
	path := ConfigPath()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// applyEnv applies environment variable overrides to the config.
func applyEnv(cfg *Config) {
	// Each provider env var only fills in a missing key (guarded by
	// `cfg.APIKey == ""`) so a key configured via the GUI/.config is never
	// silently clobbered by a stale shell variable. OPENROUTER previously
	// lacked this guard and would override an explicitly-configured key.
	if v := os.Getenv("OPENAI_API_KEY"); v != "" && cfg.APIKey == "" {
		cfg.APIKey = v
		cfg.BaseURL = "https://api.openai.com/v1"
		cfg.Provider = "openai"
	}
	if v := os.Getenv("OPENROUTER_API_KEY"); v != "" && cfg.APIKey == "" {
		cfg.APIKey = v
		cfg.BaseURL = "https://openrouter.ai/api/v1"
		cfg.Provider = "openrouter"
	}
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" && cfg.APIKey == "" {
		cfg.APIKey = v
		cfg.BaseURL = "https://api.anthropic.com/v1"
		cfg.Provider = "anthropic"
	}
	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" && cfg.APIKey == "" {
		cfg.APIKey = v
		cfg.BaseURL = "https://api.deepseek.com/v1"
		cfg.Provider = "deepseek"
		cfg.Model = "deepseek-chat"
	}
	if v := os.Getenv("DARKCODE_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("DARKCODE_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("DARKCODE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
}

// Validate checks that the config is usable. It is local-LLM-aware: when
// EnableLocalLLM is true, cloud credentials (api_key/base_url/model) are not
// required because the embedded provider supplies its own models. It also
// guards against a config whose memory_dir/projects_dir points into another
// user's home directory (a common copy-paste portability bug), and rejects
// nonsensical numeric settings. Returns a single aggregated error.
func (cfg *Config) Validate() error {
	var errs []string

	// Cloud credentials are only required when no local LLM is configured.
	if !cfg.EnableLocalLLM {
		if cfg.APIKey == "" {
			errs = append(errs, "api_key is required (set DARKCODE_API_KEY/OPENROUTER_API_KEY/OPENAI_API_KEY, or enable enable_local_llm)")
		}
		if cfg.BaseURL == "" {
			errs = append(errs, "base_url is required (or enable enable_local_llm)")
		}
		if cfg.Model == "" {
			errs = append(errs, "model is required (or enable enable_local_llm)")
		}
	}

	if cfg.ContextLength <= 0 {
		errs = append(errs, "context_length must be > 0")
	}
	if cfg.MaxTurns <= 0 {
		errs = append(errs, "max_turns must be > 0")
	}
	if cfg.MaxConcurrent <= 0 {
		errs = append(errs, "max_concurrent must be > 0")
	}

	switch cfg.RoutingMode {
	case "", "single", "escalation", "consensus":
		// ok
	default:
		errs = append(errs, "unknown routing_mode: "+cfg.RoutingMode)
	}
	switch cfg.SafetyLevel {
	case "", "off", "normal", "strict":
		// ok
	default:
		errs = append(errs, "unknown safety_level: "+cfg.SafetyLevel)
	}

	// Portability guard: an absolute memory/projects dir that lives inside a
	// different user's home is almost certainly a stale copy-paste. resolveDefaults
	// already re-homes empty strings to the current user; only warn here so we
	// never hard-fail on a valid-but-unusual layout.
	for name, dir := range map[string]string{"memory_dir": cfg.MemoryDir, "projects_dir": cfg.ProjectsDir} {
		if dir == "" {
			continue
		}
		if hint := staleHomeHint(dir); hint != "" {
			errs = append(errs, fmt.Sprintf("%s %q %s", name, dir, hint))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// staleHomeHint returns a human hint when dir appears to point into another
// user's home directory; empty string otherwise.
func staleHomeHint(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		return ""
	}
	// Only flag paths under /home/<user>/ or /Users/<user>/.
	if !strings.HasPrefix(abs, "/home/") && !strings.HasPrefix(abs, "/Users/") {
		return ""
	}
	rest := strings.TrimPrefix(abs, "/home/")
	if strings.HasPrefix(abs, "/Users/") {
		rest = strings.TrimPrefix(abs, "/Users/")
	}
	slash := strings.IndexByte(rest, '/')
	configUser := rest
	if slash >= 0 {
		configUser = rest[:slash]
	}
	myUser := filepath.Base(home)
	if configUser != "" && configUser != myUser {
		return "points into another user's home (" + configUser + "); clear it to use " + myUser + "'s default"
	}
	return ""
}
