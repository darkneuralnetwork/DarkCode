package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkcode/security"
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
	UIMode      bool   `json:"ui_mode,omitempty"`
	SafetyLevel string `json:"safety_level,omitempty"` // strict, normal, relaxed

	// Sandbox confines shell commands run by the terminal tool so they can only
	// write inside the workspace (plus a small set of build-cache dirs); the
	// rest of the filesystem is read-only. Needs bwrap or firejail installed.
	//   "off"    — never confine.
	//   "auto"   — confine when a backend is available, else run unconfined (default).
	//   "on"     — confine; if no backend is available, warn but still run.
	//   "strict" — require confinement; refuse to run commands when no backend.
	// The DARKCODE_SANDBOX env var overrides this (0=off, 1=on).
	Sandbox string `json:"sandbox,omitempty"`
	// SandboxWritable lists extra absolute paths the sandbox keeps writable
	// (e.g. a module cache like ~/go). The workspace and common caches are
	// always writable.
	SandboxWritable []string `json:"sandbox_writable,omitempty"`

	// ExecutionBackend decides where shell commands run: "local" (default,
	// confined by the sandbox), "docker" (a disposable container per command,
	// with the workspace bind-mounted), or "ssh" (a remote host).
	ExecutionBackend string `json:"execution_backend,omitempty"`
	// ExecutionImage is the container image for the docker backend.
	ExecutionImage string `json:"execution_image,omitempty"`
	// ExecutionHost is the [user@]hostname for the ssh backend, and
	// ExecutionPort its optional port.
	ExecutionHost string `json:"execution_host,omitempty"`
	ExecutionPort int    `json:"execution_port,omitempty"`

	// DenyRules refuse matching tool calls outright, ahead of every permissive
	// path — the relaxed safety level, a session-wide approval, or an approver
	// that would say yes. Each rule is a tool name, optionally followed by
	// ":pattern" matched against the call's string arguments with * and ?
	// wildcards, e.g. "terminal:*rm -rf /*", "write_file:*/.ssh/*", "git:push".
	DenyRules []string `json:"deny_rules,omitempty"`

	// SmartApproval lets an auxiliary model auto-approve routine low-risk
	// actions that the classifier flagged, to cut prompt fatigue. It can never
	// approve a high/critical action, a deny-rule match, or a call carrying a
	// secret — those always reach the user. Off by default.
	SmartApproval bool `json:"smart_approval,omitempty"`

	// ApprovalTimeoutSeconds bounds how long an approval prompt may block
	// before the call is denied. 0 uses the 5-minute default. Denying on
	// timeout keeps an unattended run from hanging forever on a prompt.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`

	// BlastRadiusThreshold escalates a file edit to require approval when the
	// code graph says that share (0..1) of the repository depends on the file,
	// even at a permissive safety level. 0 disables the check. 0.25 is a
	// sensible starting point: it catches edits to genuinely central files
	// without prompting on ordinary ones.
	BlastRadiusThreshold float64 `json:"blast_radius_threshold,omitempty"`

	// AirGap refuses every connection that leaves the machine. Loopback and
	// private addresses still work, so local model servers keep running; no
	// repository content can reach the internet.
	AirGap bool `json:"air_gap,omitempty"`

	MaxConcurrent   int  `json:"max_concurrent,omitempty"`
	CompressContext bool `json:"compress_context,omitempty"`

	// UseCtxEngine enables the intelligent context-assembly engine
	// (dedup + TF-IDF ranking + budget trimming) for the General-mode
	// fast path. Default false (raw STM append) to preserve behavior.
	UseCtxEngine bool `json:"use_ctx_engine,omitempty"`

	// HealthDaemon runs the background structural watch — import cycles
	// appearing, coupling climbing, a hotspot losing its last test. On by
	// default: it makes no model calls at all, holds itself to
	// HealthCPUPercent of a single core, and the whole point of the signal is
	// that it is the *change* that matters, which nobody sees by running a
	// report manually often enough.
	//
	// No omitempty: this is a bool that defaults to true, so omitting a
	// user's explicit `false` on save would silently turn the daemon back on
	// the next time the config was read.
	HealthDaemon bool `json:"health_daemon"`

	// HealthCPUPercent bounds the daemon to this share of a single core.
	HealthCPUPercent int `json:"health_cpu_percent,omitempty"`

	// AutoIngest indexes the active workspace into semantic memory so
	// retrieval has something to retrieve over. Without it the knowledge graph
	// filled itself from AST sync while the vector index beside it stayed
	// empty, and recall got blamed for missing what it was never shown.
	//
	// The cost is one embedding call per stored chunk, which is free on a
	// local embedder and billable on a hosted one — so it is incremental: a
	// content hash per file means only files that actually changed are
	// re-embedded, and the steady state costs a hash and nothing else.
	//
	// On by default when an embedder exists; with no embedder configured the
	// chunks are still stored and searchable by keyword, which is cheaper
	// still. No omitempty: this defaults to true, so omitting a user's
	// explicit false on save would silently switch it back on.
	AutoIngest bool `json:"auto_ingest"`

	// BackgroundWork is the one preference the three fields above were asking
	// separately: "off", "light" (keep indexes current) or "full" (also run the
	// health daemon). Empty means infer it from health_daemon/auto_ingest, so
	// an existing config keeps behaving exactly as it did.
	//
	// Read it through ResolvedBackgroundWork, IngestInBackground or
	// HealthDaemonEnabled rather than directly — see canonical.go.
	BackgroundWork string `json:"background_work,omitempty"`

	// ExecutionProfile controls DAG + consensus parallelism: "parallel",
	// "sequential" (safe on strict free-tier RPM limits), or "auto" (default:
	// sequential when only free-tier cloud models are registered).
	ExecutionProfile string `json:"execution_profile,omitempty"`

	// PlanApproval controls the interactive plan gate: "always" pauses every
	// planned task, "auto" (default) pauses only deep plans, "never" runs
	// immediately. PlanDepth overrides planning depth: "auto"/"light"/"deep".
	PlanApproval string `json:"plan_approval,omitempty"`
	PlanDepth    string `json:"plan_depth,omitempty"`

	// --- Context Compressor ---
	// The model used for context compression (Layer 3). If empty, the primary
	// model is used. The user can pick any registered model from the GUI so a
	// cheaper/faster model handles compression while the primary handles reasoning.
	CompressorModel string `json:"compressor_model,omitempty"`

	// EmbeddingModel selects the vector-embedding model for semantic memory/RAG:
	//   ""     (default) auto: the local embedded model when loaded, else off
	//          (recall falls back to keyword overlap).
	//   "off"  never embed.
	//   <name> a model from Models whose endpoint serves /embeddings.
	EmbeddingModel string `json:"embedding_model,omitempty"`

	// --- Execution strategy ---
	// Deliberately absent: agentic_loop, max_loops and post_loop_consensus.
	// Strategy is a property of a request, not of an installation — whether a
	// task should iterate depends on the task, which the config file cannot
	// know. They are now the /loop, /graph and /consensus verbs, chosen at the
	// moment they are needed. See docs/strategy-as-verbs.md, and
	// deprecatedKeys below for what happens to an older config that still
	// carries them.

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
	//   "auto"  — load only when the hardware tier allows AND the resource
	//             governor confirms the full bill fits free memory.
	//   "on"    — prefer local when safe; still refused (logged) if it won't fit.
	//   "force" — pin to local; routing never falls back to cloud. The governor
	//             still applies, so an over-budget load is refused, not silent.
	// Empty derives from EnableLocalLLM (true → "auto", false → "off").
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

	// MemoryProfile is the context/RAM knob for the local model:
	//   "lean"     — 8192 ctx: lowest RAM.
	//   "balanced" — 16384 ctx: RAG + project brief (default).
	//   "max"      — 32768 ctx: largest window, highest RAM.
	// Empty = auto. A set EmbeddedContextSize (>0) overrides this. Resolve via
	// EffectiveEmbeddedContextSize().
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

	// UseLocalForAux routes auxiliary calls (loop self-eval, context rewrite,
	// plan amend) to the local model when one is loaded and the prompt fits,
	// else cloud. Defaults true. No omitempty: an explicit false must persist.
	UseLocalForAux bool `json:"use_local_for_aux"`
	// SkipAuxForReadOnly skips the plan/workflow amend for read-only / question
	// turns (nothing to change), saving 2 cloud calls on the common case.
	SkipAuxForReadOnly bool `json:"skip_aux_for_read_only"`

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
	// APIKeys pools several credentials for this model. Calls rotate across
	// them and a key that gets throttled is parked, which is what keeps a
	// per-key free-tier quota from stalling the whole session. APIKey is used
	// as well when set, so adding keys never invalidates an existing config.
	APIKeys []string `json:"api_keys,omitempty"`
	// ReasoningEffort ("low" | "medium" | "high") is sent to models that
	// support it. Empty omits the field.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
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
		Model:                 "",
		Provider:              "embedded",
		BaseURL:               "http://127.0.0.1:0/v1",
		APIKey:                "",
		MaxTurns:              50,
		Temperature:           0.7,
		ContextLength:         16000,
		SystemPrompt:          DefaultSystemPrompt,
		RoutingMode:           "single",
		SafetyLevel:           "normal",
		Sandbox:               "auto",
		MaxConcurrent:         3,
		CompressContext:       true,
		ExecutionProfile:      "auto",
		PlanApproval:          "auto",
		PlanDepth:             "auto",
		HealthDaemon:          true,
		AutoIngest:            true,
		HealthCPUPercent:      5,
		MemoryDir:             ".darkcode/memory",
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

// ResolvedSandboxMode returns the effective sandbox mode
// ("off"|"auto"|"on"|"strict"). The DARKCODE_SANDBOX env var overrides the
// config (1 → on, 0 → off) for back-compat with the old opt-in flag.
// Unrecognized config values fall back to "auto".
func (cfg *Config) ResolvedSandboxMode() string {
	switch os.Getenv("DARKCODE_SANDBOX") {
	case "1":
		return "on"
	case "0":
		return "off"
	}
	switch cfg.Sandbox {
	case "off", "auto", "on", "strict":
		return cfg.Sandbox
	default:
		return "auto"
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

// ConfigPath returns the config file path: the system-wide
// "~/.darkcode/config.json" by default, so one install serves every directory.
// A legacy per-directory "./.config" is honored as a migration fallback only
// when the system-wide config doesn't exist yet.
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
	warnDeprecatedKeys(data)

	// Environment variables override config file
	applyEnv(cfg)

	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = 50
	}
	if cfg.ContextLength == 0 {
		cfg.ContextLength = 16000
	}
	// Older config files predate the planning-phase fields: default them so
	// existing installs get the approval gate + adaptive depth.
	if cfg.PlanApproval == "" {
		cfg.PlanApproval = "auto"
	}
	if cfg.PlanDepth == "" {
		cfg.PlanDepth = "auto"
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
	// Write the canonical form: one field per question, rather than the two or
	// three that a resolver has to reconcile. See canonical.go.
	out := cfg.canonical()
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// applyEnv applies environment variable overrides to the config.
func applyEnv(cfg *Config) {
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
	cfg.resolveSecretRefs()
}

// resolveSecretRefs replaces any "op://", "bw://" or "pass://" credential with
// the value fetched from the password manager, so a key can live in a vault
// instead of in plaintext on disk. A failed lookup leaves the reference in
// place and warns: the resulting auth error names the real problem, whereas a
// silently blanked key would look like a missing config.
func (cfg *Config) resolveSecretRefs() {
	resolve := func(field, value string) string {
		if !security.IsSecretRef(value) {
			return value
		}
		secret, err := security.ResolveSecret(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not resolve %s from the password manager: %v\n", field, err)
			return value
		}
		return secret
	}

	cfg.APIKey = resolve("api_key", cfg.APIKey)
	for name, mc := range cfg.Models {
		mc.APIKey = resolve("models."+name+".api_key", mc.APIKey)
		for i, k := range mc.APIKeys {
			mc.APIKeys[i] = resolve("models."+name+".api_keys", k)
		}
		cfg.Models[name] = mc
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

// deprecatedKeys are settings that no longer exist, and what replaced them.
//
// Go's json.Unmarshal ignores unknown fields, so removing a field would
// otherwise mean a user's setting silently stops having any effect — no error,
// no warning, just different behaviour they cannot account for. That is the
// worst way to retire a setting, so retiring one says so out loud.
var deprecatedKeys = map[string]string{
	"agentic_loop":        "type /loop before a task to iterate on it, or /always loop to keep doing so",
	"max_loops":           "completion is decided by acceptance checks now, and the iteration ceiling is an internal backstop",
	"post_loop_consensus": "type /consensus before a question to answer it with every registered model",
}

// warnDeprecatedKeys reports settings in a loaded config file that no longer do
// anything. Best-effort: a config that fails this second parse has already been
// accepted by the first, and a warning is not worth failing a startup over.
func warnDeprecatedKeys(data []byte) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	// Sorted so the output is stable across runs; map order would otherwise
	// shuffle the notes and make them look like different messages.
	keys := make([]string, 0, len(deprecatedKeys))
	for k := range deprecatedKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, present := raw[key]; present {
			fmt.Fprintf(os.Stderr,
				"note: %q in your config no longer does anything — %s\n", key, deprecatedKeys[key])
		}
	}
}
