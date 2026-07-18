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

	// --- Embeddings (local-first upgrade Phase C) ---
	// The model used to generate vector embeddings for semantic memory/RAG.
	//   ""      (default) auto: use the local embedded model when one is
	//           loaded (llama-server already runs with --embedding), else
	//           embeddings stay off and recall uses keyword overlap.
	//   "off"   never embed, even when a local model is loaded.
	//   <name>  a model from Models to use for embeddings (its endpoint must
	//           serve /embeddings; note a cloud model here incurs per-write
	//           and per-query API cost).
	EmbeddingModel string `json:"embedding_model,omitempty"`

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
	// LocalMode refines EnableLocalLLM with never-force semantics:
	//   "off"   — never load the local model.
	//   "auto"  — load only when the hardware tier allows it AND the Local
	//             Resource Governor confirms the full bill (model + KV cache +
	//             LoRAs + overhead) fits free memory.
	//   "on"    — prefer local whenever safe; still refuses (with a logged
	//             reason) when the governor says it doesn't fit — no
	//             configuration value may launch an over-budget process, that
	//             is what hangs low-RAM machines.
	//   "force" — pin execution to the local model: routing NEVER falls back
	//             to a cloud provider (a request fails loudly rather than
	//             silently going remote), and the local model is auto-started
	//             on demand. The Resource Governor still applies — "force"
	//             skips the hardware-tier gate like "on", but an over-budget
	//             load is still refused with a diagnostic (that refusal
	//             surfaces as a clear error, never a silent cloud fallback).
	//             See router.SetForceLocal and Config.ForceLocal.
	// Empty = derive from EnableLocalLLM (true → "auto", false → "off") so
	// existing configs keep working unchanged.
	LocalMode string `json:"local_mode,omitempty"`
	// Toggle whether to offload simple tasks (explain error, code review) to local LLM.
	EnableLocalOffloading bool `json:"enable_local_offloading"`
	// LocalModelRole is the consensus role assigned to the local/embedded model
	// (critic, skeptic, knowledge_booster, …). Empty = no explicit role (the
	// model stays at its size-tier: medium_local/tiny_local). Unlike cloud
	// models (whose role lives in ModelConfig.Role), the local model is a
	// runtime entity not stored in the Models map, so its role needs its own
	// field to survive restarts.
	LocalModelRole string `json:"local_model_role,omitempty"`

	// MemoryProfile is the user-facing curated context/RAM knob for the local
	// model, so users pick an intent instead of guessing a raw -c value (a
	// too-small raw value silently truncates injected context and breaks even
	// simple tasks — the reason this abstraction exists):
	//   "lean"     — 8192 ctx: lowest RAM, fine for chat + small coding.
	//   "balanced" — 16384 ctx: comfortable for RAG + a project brief (default).
	//   "max"      — 32768 ctx: largest window, highest RAM.
	// Empty = auto (governor's RAM-aware default). EmbeddedContextSize, when
	// set (>0), always wins over the profile — the power-user escape hatch.
	// Resolve via EffectiveEmbeddedContextSize().
	MemoryProfile string `json:"memory_profile,omitempty"`

	// EmbeddedContextSize overrides the llama-server context window (-c) for
	// the local model. 0 = auto (RAM-aware default from computeLaunchOpts:
	// ≥ 32768 on systems with ≥ 4GB RAM). >0 = always use this value, winning
	// over the RAM guard AND over MemoryProfile. Useful for forcing a
	// larger/smaller context than the auto-detected default.
	EmbeddedContextSize int `json:"embedded_context_size,omitempty"`

	// EmbeddedIdleTimeoutMinutes unloads the local model after this many
	// minutes of inactivity, freeing RAM/VRAM. Fresh installs default to 15
	// (an idle multi-GB model shouldn't hold RAM hostage indefinitely);
	// 0 in an existing config keeps the model resident (legacy behavior,
	// and the explicit way to disable idle unload).
	EmbeddedIdleTimeoutMinutes int `json:"embedded_idle_timeout_minutes,omitempty"`

	// --- Auxiliary-call routing (cost reduction) ---
	// UseLocalForAux routes behind-the-scenes calls (loop self-eval, context
	// rewrite, plan/workflow amend) to the local model when one is loaded and
	// healthy and the prompt fits its window — otherwise cloud. Safe by
	// construction: with no local model this is a no-op (pure cloud), so it
	// never forces local. Defaults on (true) via applied defaults.
	// (no omitempty: an explicit false must persist, and an absent field in an
	// older config decodes over the DefaultConfig true.)
	UseLocalForAux bool `json:"use_local_for_aux"`
	// SkipAuxForReadOnly skips the plan/workflow amend for read-only / question
	// turns (nothing to change), saving 2 cloud calls on the common case.
	SkipAuxForReadOnly bool `json:"skip_aux_for_read_only"`
	// PostLoopConsensus re-runs consensus over an already-complete loop answer.
	// Off by default: it is N+1 extra cloud calls to polish a finished answer.
	PostLoopConsensus bool `json:"post_loop_consensus,omitempty"`

	// --- Cost governor ---
	// Spend caps (USD) enforced against accumulated LLM cost. 0 = unlimited.
	// Because local models cost nothing, these only ever constrain cloud
	// spend. CostLimitAction selects what happens when a cap is reached:
	// "warn" (default — log/surface but proceed) or "block" (refuse new
	// requests). Both default to no enforcement (caps unset).
	CostLimitPerSessionUSD float64 `json:"cost_limit_per_session_usd,omitempty"`
	CostLimitPerDayUSD     float64 `json:"cost_limit_per_day_usd,omitempty"`
	CostLimitAction        string  `json:"cost_limit_action,omitempty"` // "warn" | "block"

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
// MemoryProfileContext maps a memory-profile name to a llama-server context
// window (-c). Unknown/empty returns 0, meaning "auto" (let the governor pick
// its RAM-aware default). Exposed so the UI and tests share one mapping.
func MemoryProfileContext(profile string) int {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "lean":
		return 8192
	case "balanced":
		return 16384
	case "max":
		return 32768
	default:
		return 0
	}
}

// EffectiveEmbeddedContextSize resolves the local model's context window from
// (in priority order) the explicit EmbeddedContextSize override, then the
// MemoryProfile, then 0 (auto). This single resolver is used everywhere the
// context size is consumed so the profile and the raw override never disagree.
func (c *Config) EffectiveEmbeddedContextSize() int {
	if c.EmbeddedContextSize > 0 {
		return c.EmbeddedContextSize
	}
	return MemoryProfileContext(c.MemoryProfile)
}

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
		// Fresh installs free an idle local model's RAM after 15 minutes; an
		// existing config's explicit 0 (or absence, decoded over defaults)
		// keeps legacy stay-resident behavior.
		EmbeddedIdleTimeoutMinutes: 15,
		// Auxiliary-call cost savings default on; safe because they only take
		// effect when a healthy local model exists (else pure cloud, unchanged).
		UseLocalForAux:     true,
		SkipAuxForReadOnly: true,
		PostLoopConsensus:  false,
		// Fresh installs get a balanced 16384 window: comfortable for RAG + a
		// project brief without the 32768 auto-default's higher RAM. Existing
		// configs (empty profile) keep the auto behavior.
		MemoryProfile: "balanced",
	}
}

// ResolvedLocalMode returns the effective local-LLM mode
// ("off"|"auto"|"on"|"force"). LocalMode wins when set; otherwise the legacy
// EnableLocalLLM bool maps true → "auto" (capability- and budget-gated, never
// forced) and false → "off". Unrecognized values fall back to "auto" rather
// than "on"/"force" so a typo can never force-load.
func (cfg *Config) ResolvedLocalMode() string {
	switch cfg.LocalMode {
	case "off", "auto", "on", "force":
		return cfg.LocalMode
	case "":
		if cfg.EnableLocalLLM {
			return "auto"
		}
		return "off"
	default:
		if cfg.EnableLocalLLM {
			return "auto"
		}
		return "off"
	}
}

// ForceLocal reports whether the user has pinned execution to the local model
// (LocalMode "force"). When true, the router must refuse to fall back to any
// cloud provider (router.SetForceLocal) and the local model is auto-started
// on demand — the request fails with a diagnostic rather than silently going
// remote if the local model can't be brought up.
func (cfg *Config) ForceLocal() bool {
	return cfg.ResolvedLocalMode() == "force"
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
// The config lives in a system-wide "~/.darkcode/config.json" by default, so
// one install serves every directory the binary is launched from — matching
// where MemoryDir/ProjectsDir already default to (see Load below) and where
// the local llama-server binary/models/LoRAs live (app_wireup.go's
// resolveDataDir). An existing per-directory install (a "./.config" file
// from before this consolidation) is honored as a migration fallback so it
// keeps working without the user having to move anything: it's only used
// when the system-wide config doesn't exist yet. A brand-new install always
// lands on the system-wide path.
func ConfigPath() string {
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		homePath := filepath.Join(home, ".darkcode", "config.json")
		if _, err := os.Stat(homePath); err == nil {
			return homePath
		}
		if cwd, err := os.Getwd(); err == nil {
			if cwdPath := filepath.Join(cwd, ".config"); fileExists(cwdPath) {
				return cwdPath
			}
		}
		return homePath
	}
	// No resolvable home directory (unusual, e.g. a minimal container) —
	// fall back to the previous CWD-relative behavior entirely.
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, ".config")
	}
	return filepath.Join(".", ".darkcode", "config.json")
}

// fileExists reports whether path exists and is readable enough to stat.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	// Unlike the legacy CWD "./.config" file (whose parent, the CWD, always
	// exists), the system-wide "~/.darkcode/" directory may not exist yet on
	// a fresh install — create it so the first Save() doesn't fail.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create config directory: %w", err)
		}
	}
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
