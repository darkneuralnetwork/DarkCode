package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/agents"
	"github.com/darkcode/compression"
	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/ctxengine"
	"github.com/darkcode/llm"
	"github.com/darkcode/loop"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/permission"
	"github.com/darkcode/plan"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
)

// Layer 1 — the orchestration kernel. Interprets intent, decomposes tasks into
// DAGs, delegates to sub-agents, routes models, enforces safety, and stores
// episodic memory. Trivial tasks are answered directly without decomposition.

// Kernel is the orchestration core — Layer 1.
type Kernel struct {
	cfg        Config
	router     *router.Router
	registry   *tools.Registry
	memory     *memory.System
	retriever  *memory.HybridRetriever // ranked recall over episodic+semantic+KG
	compressor *compression.Compressor
	factory    *agents.AgentFactory
	executor   *agents.ConcurrentExecutor
	emitter    *ui.EventEmitter
	verifier   *agents.VerificationPipeline
	agentBus   *agents.AgentBus

	// permission gate — enforces user approval for dangerous tool calls.
	// The registry consults it before executing any tool.
	gate *permission.Gate

	// modeApprover is the single mode-aware approval router installed on the
	// gate. It delegates each prompt to the GUI ServerApprover or the CLI
	// terminal approver based on the active UI mode, so switching surfaces
	// never leaves a stale approver (the prior CLI→GUI permission bug).
	modeApprover *permission.ModeAwareApprover

	// agenticLoop is the optional ReAct execution loop (looping technology).
	// Non-nil always; activated/deactivated via SetAgenticLoop.
	agenticLoop *loop.ReActLoop
	agenticOn   bool

	// requestLoop is a per-request override of the agentic-loop decision.
	// nil ⇒ fall back to the master toggle (agenticOn). The web chat sets
	// this from chat_mode=="loop" so the loop runs only when the user
	// explicitly picks Loop mode; the CLI/single-query path leaves it nil so
	// the master toggle still drives loop usage there. Guarded by mu.
	requestLoop *bool

	// requestToolsDisabled is a per-request override of tool access. nil ⇒
	// tools enabled (the default). The web chat sets this from
	// chat_mode=="general" so General mode is a fast pure-conversation path
	// with NO tools offered to the LLM (no DAG, no worker agents, no approval
	// popups); project/auto/loop leave it nil (tools on). Guarded by mu.
	requestToolsDisabled *bool

	// requestReadOnly is a per-request override for Chat mode: only read-only
	// tools (read/list/search/web) are offered and any mutating tool is
	// refused, so Chat can answer from the project without writing. nil ⇒ not
	// read-only. Set from chat_mode=="general"/"chat". Guarded by mu.
	requestReadOnly *bool

	// projectPlan/projectWorkflow hold the active project's plan + workflow for
	// the current request, set via SetProjectContext and injected into the goal
	// so every execution path follows the plan. Guarded by mu; capped at
	// maxPlanInjectBytes each.
	projectPlan     string
	projectWorkflow string

	// pendingPlan holds a plan proposal awaiting the user's approve/revise/
	// reject decision (see plan_gate.go). lastRunPlan is the most recently
	// executed graph, with final node statuses, retained until the server
	// consumes it for project persistence (ConsumeApprovedPlan). Both
	// guarded by mu.
	pendingPlan *pendingPlanState
	lastRunPlan *plan.Graph

	// lastCompressedLen tracks the STM length at the most recent compression.
	// Used to skip re-compressing the same window twice when two requests land
	// while STM is between thresholds (see compressionMinGrowth). Guarded by mu.
	lastCompressedLen int

	// ctxEngine is the optional intelligent context-assembly engine
	// ("Strategy 6b" — dedup + TF-IDF ranking + budget trimming), lazily
	// constructed by getCtxEngine when cfg.UseCtxEngine is true. nil when
	// disabled (the default), in which case callers fall back to raw STM
	// append — see executeDirectNoTools.
	ctxEngine   *ctxengine.Engine
	ctxEngineMu sync.Mutex

	// governor enforces optional spend caps before each Execute. nil = no
	// enforcement (the default), so behavior is unchanged unless a budget is
	// configured. Set via SetCostGovernor.
	governor *metrics.CostGovernor

	// classifier picks the cognition-cascade entry rung per query (see
	// cascade.go). Constructed in New; never nil in a kernel built via New.
	classifier *router.TaskClassifier

	mu         sync.Mutex
	taskLog    []TaskLogEntry
	cascadeLog []CascadeEntry // per-query rung telemetry (see cascade.go)

	// Cascade calibration state (see cascade.go). Thresholds start at
	// cascadeDefaultThreshold and only ever rise, driven by the re-ask
	// negative-label signal; counters feed CascadeStats. All guarded by mu.
	cascadeThresholds   [6]float64
	cascadeRungAnswered [6]int
	cascadeRungRetried  [6]int
	cascadeLogPath      string // "" = no persistence

	// localLoader brings up the embedded local model on demand (injected by the
	// app wireup, which owns embedded loading). Lets /local and the GUI toggle
	// start it without a restart. nil in tests. Its own mutex so a slow load
	// never blocks Execute's short critical sections on mu.
	localLoaderMu sync.Mutex
	localLoader   func(context.Context) error
}

// SetLocalLoader installs the on-demand embedded-model loader (see the
// localLoader field). Pass nil to clear it.
func (k *Kernel) SetLocalLoader(fn func(context.Context) error) {
	k.localLoaderMu.Lock()
	k.localLoader = fn
	k.localLoaderMu.Unlock()
}

// ApplyLocalPreference re-applies the config's local-LLM preference at runtime
// (pinning force-local routing and loading a local model when wanted but none
// is registered). In force mode a load failure is returned so the caller can
// surface it — force never falls back to cloud; in auto/on the error is
// advisory since a cloud model still serves.
func (k *Kernel) ApplyLocalPreference(ctx context.Context, cfg *config.Config) error {
	k.router.SetForceLocal(cfg.ForceLocal())

	if cfg.ResolvedLocalMode() == "off" || k.router.HasLocalModel() {
		return nil
	}

	k.localLoaderMu.Lock()
	loader := k.localLoader
	k.localLoaderMu.Unlock()
	if loader == nil {
		if cfg.ForceLocal() {
			return fmt.Errorf("force-local requested but on-demand local loading is not available here — restart with the local model enabled")
		}
		return nil
	}
	if err := loader(ctx); err != nil {
		if cfg.ForceLocal() {
			return fmt.Errorf("force-local: local model failed to start: %w (no cloud fallback will be used)", err)
		}
		return err
	}
	if cfg.ForceLocal() && !k.router.HasLocalModel() {
		return fmt.Errorf("force-local is active but no local model came up — check the logs; no cloud fallback will be used")
	}
	return nil
}

// SetCostGovernor installs an optional spend-cap enforcer consulted at the
// start of every Execute. Passing nil disables enforcement.
func (k *Kernel) SetCostGovernor(g *metrics.CostGovernor) {
	k.mu.Lock()
	k.governor = g
	k.mu.Unlock()
}

// getCtxEngine lazily builds the ctxengine.Engine when cfg.UseCtxEngine is
// enabled, and returns nil otherwise. Safe for concurrent use. This restores
// the "Strategy 6b" integration for the General-mode fast path
// (executeDirectNoTools): dedup + rank + budget-trim the conversation instead
// of dumping raw STM into the prompt.
func (k *Kernel) getCtxEngine() *ctxengine.Engine {
	if !k.cfg.UseCtxEngine {
		return nil
	}
	k.ctxEngineMu.Lock()
	defer k.ctxEngineMu.Unlock()
	if k.ctxEngine == nil {
		// nil LLM client: use the deterministic extractive summarizer
		// fallback rather than spending an extra LLM call on every General
		// mode request just to compress context.
		k.ctxEngine = ctxengine.NewEngine(nil)
	}
	return k.ctxEngine
}

// TaskLogEntry records a single step in the execution loop.
type TaskLogEntry struct {
	Step      string
	Timestamp time.Time
	Detail    string
}

// New creates the orchestration kernel with all layers wired together.
func New(cfg Config, rtr *router.Router, reg *tools.Registry, mem *memory.System, comp *compression.Compressor, emitter *ui.EventEmitter) *Kernel {
	errMgr := NewErrorManager()
	factory := agents.NewAgentFactory(rtr, reg, emitter, errMgr)
	executor := agents.NewConcurrentExecutor(factory, cfg.MaxConcurrent, emitter)
	verifier := agents.NewVerificationPipeline(rtr, emitter, "")
	bus := agents.NewAgentBus()

	// Create the permission gate from the configured safety level and wire it
	// into the tool registry so every tool call is checked before execution.
	gate := permission.NewGate(permissionLevelFromSafety(cfg.SafetyLevel))
	if reg != nil {
		reg.SetPermissionGate(gate)
		if emitter != nil {
			reg.SetEventEmitter(emitter)
		}
	}

	k := &Kernel{
		cfg:         cfg,
		router:      rtr,
		registry:    reg,
		memory:      mem,
		retriever:   memory.NewHybridRetriever(mem, mem.KG()),
		compressor:  comp,
		factory:     factory,
		executor:    executor,
		emitter:     emitter,
		verifier:    verifier,
		agentBus:    bus,
		gate:        gate,
		agenticLoop: loop.New(rtr, reg, emitter, cfg.MaxLoops),
		agenticOn:   cfg.AgenticLoop,
		classifier:  router.NewTaskClassifier(),
	}
	for i := range k.cascadeThresholds {
		k.cascadeThresholds[i] = cascadeDefaultThreshold
	}
	return k
}

// Gate returns the kernel's permission gate (creating a default one lazily if
// it was not set at construction time).
func (k *Kernel) Gate() *permission.Gate {
	if k.gate == nil {
		k.gate = permission.NewGate(permissionLevelFromSafety(k.cfg.SafetyLevel))
		if k.registry != nil {
			k.registry.SetPermissionGate(k.gate)
		}
	}
	return k.gate
}

// SetModeApprover installs the mode-aware approval router. The same instance
// is installed on the gate as its Approver; CLI/GUI entry points flip its mode
// instead of overwriting the gate's approver.
func (k *Kernel) SetModeApprover(ma *permission.ModeAwareApprover) {
	k.modeApprover = ma
}

// ModeApprover returns the mode-aware approval router (nil if none set). The
// console uses this to install its terminal delegate and switch to CLI mode;
// the GUI loop uses it to switch to GUI mode.
func (k *Kernel) ModeApprover() *permission.ModeAwareApprover {
	return k.modeApprover
}

// SetPermissionGate replaces the kernel's permission gate and wires it into
// the tool registry. The approver callback should be installed separately
// (e.g. by the CLI console for interactive prompting).
func (k *Kernel) SetPermissionGate(g *permission.Gate) {
	k.gate = g
	if k.registry != nil {
		k.registry.SetPermissionGate(g)
	}
}

// SetChangeRecorder wires a change recorder into the tool registry so that
// before/after state for mutating tools is captured.
func (k *Kernel) SetChangeRecorder(rec *tools.ChangeRecorder) {
	if k.registry != nil {
		k.registry.SetChangeRecorder(rec)
	}
}

// SetApprovalCallback is a legacy bridge: it wraps the simple bool callback
// as a permission.Approver on the gate. Prefer SetPermissionGate +
// Gate().SetApprover for the full allow-once / allow-session / deny flow.
func (k *Kernel) SetApprovalCallback(cb func(action string) bool) {
	if cb == nil {
		return
	}
	g := k.Gate()
	g.SetApprover(func(req permission.ApprovalRequest) permission.Verdict {
		if cb(req.Summary) {
			return permission.AllowV(permission.DecisionAllowOnce)
		}
		return permission.DenyV("")
	})
}

// SetAgenticLoop hot-toggles the optional ReAct execution loop at runtime
// (called from the Settings tab via the server). maxLoops <= 0 leaves the
// current ceiling unchanged.
func (k *Kernel) SetAgenticLoop(enabled bool, maxLoops int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.agenticOn = enabled
	k.cfg.AgenticLoop = enabled
	if maxLoops > 0 {
		k.cfg.MaxLoops = maxLoops
		if k.agenticLoop != nil {
			k.agenticLoop.SetMaxLoops(maxLoops)
		}
	}
}

// ApplyRequestOverrides applies per-request routing/safety/loop/tool overrides
// to the live router, gate, and kernel flags, returning a restore func to defer.
// Empty strings leave a setting unchanged; loop is "on"/"off"/"", tools is
// "off"/""/"on". Lets the mode/safety/chat_mode fields of POST /api/chat apply
// for a single request.
func (k *Kernel) ApplyRequestOverrides(mode, safety, loop, tools, brain string) func() {
	var oldMode core.RoutingMode
	var oldLevel permission.Level
	var oldReqLoop *bool
	var oldToolsDisabled *bool
	var oldReadOnly *bool
	var oldForceLocal *bool
	haveMode := mode != ""
	haveSafety := safety != ""
	haveLoop := loop != ""
	haveTools := tools != ""
	// Brain selector (per-request): "local" pins to the local model (offline),
	// "cloud" allows cloud routing, "auto"/"" leave the configured default. Only
	// local/cloud actually change the router's force-local flag.
	haveBrain := false
	if k.router != nil {
		switch strings.ToLower(brain) {
		case "local":
			cur := k.router.ForceLocal()
			oldForceLocal = &cur
			k.router.SetForceLocal(true)
			haveBrain = true
		case "cloud":
			cur := k.router.ForceLocal()
			oldForceLocal = &cur
			k.router.SetForceLocal(false)
			haveBrain = true
		}
	}
	if haveMode && k.router != nil {
		oldMode = k.router.GetMode()
		k.router.SetMode(parseRoutingModeLocal(mode))
	}
	if haveSafety && k.gate != nil {
		oldLevel = k.gate.Level()
		k.gate.SetLevel(permission.LevelFromString(safety))
	}
	if haveLoop || haveTools {
		k.mu.Lock()
		if haveLoop {
			oldReqLoop = k.requestLoop
			switch strings.ToLower(loop) {
			case "on":
				on := true
				k.requestLoop = &on
			case "off":
				off := false
				k.requestLoop = &off
			}
		}
		if haveTools {
			oldToolsDisabled = k.requestToolsDisabled
			oldReadOnly = k.requestReadOnly
			disabled, enabled, readOnly := true, false, true
			switch strings.ToLower(tools) {
			case "off":
				k.requestToolsDisabled = &disabled
				k.requestReadOnly = nil
			case "readonly", "read-only":
				// Chat mode: tools enabled, but only read-only ones offered.
				k.requestToolsDisabled = &enabled
				k.requestReadOnly = &readOnly
			case "on":
				k.requestToolsDisabled = &enabled
				k.requestReadOnly = nil
			}
		}
		k.mu.Unlock()
	}
	return func() {
		if haveMode && k.router != nil {
			k.router.SetMode(oldMode)
		}
		if haveSafety && k.gate != nil {
			k.gate.SetLevel(oldLevel)
		}
		if haveBrain && k.router != nil && oldForceLocal != nil {
			k.router.SetForceLocal(*oldForceLocal)
		}
		if haveLoop || haveTools {
			k.mu.Lock()
			if haveLoop {
				k.requestLoop = oldReqLoop
			}
			if haveTools {
				k.requestToolsDisabled = oldToolsDisabled
				k.requestReadOnly = oldReadOnly
			}
			k.mu.Unlock()
		}
	}
}

// loopEnabledForRequest reports whether the ReAct loop should run for the
// current request. A per-request override (set by the web chat's Loop mode)
// wins; otherwise the master toggle (agenticOn) decides — preserving the
// CLI/single-query behaviour where the loop runs iff the user enabled it in
// Settings. Mutex-safe.
func (k *Kernel) loopEnabledForRequest() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.requestLoop != nil {
		return *k.requestLoop
	}
	return k.agenticOn
}

// toolsDisabledForRequest reports whether tool access is disabled for the
// current request (General mode fast path). When true, Execute takes a
// lightweight single-call path with NO tools offered to the LLM — no DAG,
// no worker agents, no approval popups. Mutex-safe.
func (k *Kernel) toolsDisabledForRequest() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.requestToolsDisabled != nil {
		return *k.requestToolsDisabled
	}
	return false
}

// readOnlyForRequest reports whether this is a Chat (read-only) request: only
// read-only tools are offered and mutating tools are refused. Mutex-safe.
func (k *Kernel) readOnlyForRequest() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.requestReadOnly != nil && *k.requestReadOnly
}

// AgenticLoopEnabled reports whether the ReAct loop is currently active.
func (k *Kernel) AgenticLoopEnabled() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.agenticOn
}

// ReloadModels re-wires the model router with the latest config so that models
// added/removed/switched via the UI take effect immediately, without restart.
func (k *Kernel) ReloadModels(cfg *config.Config) {
	// Sync the routing mode from config so UI changes to routing_mode take
	// effect immediately (not just at startup).
	k.router.SetMode(parseRoutingModeLocal(cfg.RoutingMode))

	// Register all models from the config map. RegisterModel dedups by name
	// (upsert), so the primary — which also appears in cfg.Models — is not
	// duplicated in the router's allModels slice. The tier map keeps the last
	// writer per tier (used by Route/escalation); the allModels slice keeps
	// every registered model (used by consensus fan-out).
	for _, mc := range cfg.Models {
		client := llm.NewClient(mc.BaseURL, mc.APIKey, mc.Model)
		client.SetProvider(mc.Provider)
		k.router.RegisterModel(modelTierFromString(mc.Tier), client, mc.Model)
		// Set the consensus role from config (empty = default "general").
		k.router.SetModelRole(mc.Model, mc.Role)
	}

	// Register the primary model at the mode-appropriate tier (reasoning for
	// consensus, coding otherwise). This ensures the primary wins its tier
	// slot for Route/escalation, and MarkPrimary below flags it as the
	// consensus synthesizer.
	primaryClient := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	primaryClient.SetProvider(cfg.Provider)
	tier := primaryTierForMode(k.router.GetMode())
	k.router.RegisterModel(tier, primaryClient, cfg.Model)
	k.router.MarkPrimary(cfg.Model)

	// Re-wire the context compressor with the user-selected model (if any),
	// or fall back to the primary. This makes a compressor-model change made
	// via the GUI take effect immediately, without restart.
	if k.compressor != nil {
		compClient := primaryClient
		compModel := cfg.Model
		if cfg.CompressorModel != "" {
			if mc, ok := cfg.Models[cfg.CompressorModel]; ok {
				compClient = llm.NewClient(mc.BaseURL, mc.APIKey, mc.Model)
				compClient.SetProvider(mc.Provider)
				compModel = mc.Model
			}
		}
		k.compressor.SetClient(compClient, compModel)
		k.log("compression", "Context compressor model: "+compModel+
			" (user-selected="+nonEmpty(cfg.CompressorModel, "<primary>")+")")
	}
}

// CompressProjectContext produces a concise briefing of a project's raw
// context.md using the compressor model, so a reopened project gets a short
// summary instead of a huge raw log. Nil-safe wrapper over Compressor.Summarize.
func (k *Kernel) CompressProjectContext(ctx context.Context, content, projectName string) (string, error) {
	if k.compressor == nil || strings.TrimSpace(content) == "" {
		return "", nil
	}
	return k.compressor.Summarize(ctx, content, projectName)
}

// nonEmpty returns s, or fallback if s is empty.
func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// parseRoutingModeLocal mirrors main.parseRoutingMode without the import
// cycle (main depends on orchestrator, not vice-versa).
func parseRoutingModeLocal(s string) core.RoutingMode {
	switch strings.ToLower(s) {
	case "escalation":
		return core.RouteEscalation
	case "consensus":
		return core.RouteConsensus
	default:
		return core.RouteSingle
	}
}

// primaryTierForMode picks the tier used for the primary model given a routing mode.
func primaryTierForMode(mode core.RoutingMode) core.ModelTier {
	switch mode {
	case core.RouteConsensus:
		return core.ModelTierReasoning
	case core.RouteEscalation:
		return core.ModelTierCoding
	default:
		return core.ModelTierCoding
	}
}

// modelTierFromString maps a config tier label to a ModelTier.
func modelTierFromString(s string) core.ModelTier {
	switch strings.ToLower(s) {
	case "reasoning":
		return core.ModelTierReasoning
	case "fast":
		return core.ModelTierFast
	case "local":
		return core.ModelTierLocal
	case "critic":
		return core.ModelTierCritic
	default:
		return core.ModelTierCoding
	}
}

// permissionLevelFromSafety maps the orchestrator SafetyLevel to a permission
// gate level.
func permissionLevelFromSafety(s SafetyLevel) permission.Level {
	switch s {
	case SafetyStrict:
		return permission.LevelStrict
	case SafetyRelaxed:
		return permission.LevelRelaxed
	default:
		return permission.LevelNormal
	}
}

// ============================================================================
// SAFETY — Check if an action requires approval
// ============================================================================

// RequiresApproval reports whether the given tool call would need user
// approval under the current safety level. It does not prompt or execute.
func (k *Kernel) RequiresApproval(tool string, args map[string]interface{}) bool {
	g := k.Gate()
	level := g.Level()
	if level == permission.LevelRelaxed {
		return false
	}
	_, dangerous := permission.ClassifyExported(tool, args)
	if level == permission.LevelStrict {
		return true
	}
	return dangerous
}

// GateStats returns counters for the permission gate (asked/approved/denied).
func (k *Kernel) GateStats() permission.Stats {
	return k.Gate().Stats()
}

// ============================================================================
// LOGGING
// ============================================================================

func (k *Kernel) log(step, detail string) {
	k.mu.Lock()
	k.taskLog = append(k.taskLog, TaskLogEntry{
		Step:      step,
		Timestamp: time.Now(),
		Detail:    detail,
	})
	k.mu.Unlock()

	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("kernel", step, detail)
	}
}

// GetTaskLog returns the execution log.
func (k *Kernel) GetTaskLog() []TaskLogEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	result := make([]TaskLogEntry, len(k.taskLog))
	copy(result, k.taskLog)
	return result
}

// ============================================================================
// STATUS — Current system state summary
// ============================================================================

func (k *Kernel) Status() string {
	return fmt.Sprintf(
		"Orchestrator Kernel:\n"+
			"  Routing mode: %s\n"+
			"  UI mode: %v\n"+
			"  Safety level: %d\n"+
			"  Max concurrent: %d\n"+
			"  Compress context: %v\n"+
			"  Task log entries: %d\n"+
			"\n%s",
		k.cfg.RoutingMode,
		k.cfg.UIMode,
		k.cfg.SafetyLevel,
		k.cfg.MaxConcurrent,
		k.cfg.CompressContext,
		len(k.taskLog),
		k.memory.Summary(),
	)
}
