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
	"github.com/darkcode/permission"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
)

// ============================================================================
// LAYER 1 — ORCHESTRATION KERNEL
//
// The Kernel is the central intelligence of the system. It:
//   - Interprets user intent
//   - Decomposes tasks into DAGs
//   - Delegates to sub-agents
//   - Selects models dynamically via the router
//   - Enforces execution safety
//   - Merges final outputs
//   - Stores episodic memory after each task
//   - Extracts reusable skills (self-improvement)
//
// It NEVER directly solves everything alone unless the task is trivial.
// ============================================================================

// Config holds all configuration for the orchestrator kernel.
type Config struct {
	RoutingMode      core.RoutingMode
	UIMode           bool
	MaxConcurrent    int
	ExecutionProfile string
	MaxTurns         int
	SafetyLevel      SafetyLevel
	CompressContext  bool
	UseCtxEngine     bool
	ContextLength    int

	// AgenticLoop enables the optional ReAct (Sense-Think-Act) execution loop
	// from the looping_tech design. When true, Execute delegates to the loop
	// package instead of the DAG decomposition.
	AgenticLoop bool
	MaxLoops    int
}

// SafetyLevel controls how restrictive the safety checks are.
type SafetyLevel int

const (
	SafetyStrict  SafetyLevel = iota // require approval for all tool use
	SafetyNormal                     // require approval for destructive actions only
	SafetyRelaxed                    // auto-approve everything (sandboxed mode)
)

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		RoutingMode:     core.RouteSingle,
		UIMode:          false,
		MaxConcurrent:   3,
		MaxTurns:        10,
		SafetyLevel:     SafetyNormal,
		CompressContext: true,
	}
}

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

	// projectPlan/projectWorkflow hold the active project's implementation
	// plan + workflow architecture for the CURRENT request. The server/CLI
	// set these via SetProjectContext before calling Execute (when a project
	// is active) and clear them via ClearProjectContext afterwards. They are
	// injected into the goal at the start of Execute so the planner, the
	// ReAct loop, AND the direct paths all follow the plan — previously the
	// plan/workflow was a write-only display artifact generated AFTER
	// execution and never fed forward. Guarded by mu. Capped at
	// maxPlanInjectBytes each to bound context growth.
	projectPlan     string
	projectWorkflow string

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

	mu      sync.Mutex
	taskLog []TaskLogEntry
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

	return &Kernel{
		cfg:        cfg,
		router:     rtr,
		registry:   reg,
		memory:     mem,
		retriever:  memory.NewHybridRetriever(mem, mem.KG()),
		compressor: comp,
		factory:    factory,
		executor:   executor,
		emitter:    emitter,
		verifier:   verifier,
		agentBus:   bus,
		gate:       gate,
		agenticLoop: loop.New(rtr, reg, emitter, cfg.MaxLoops),
		agenticOn:  cfg.AgenticLoop,
	}
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

// ApplyRequestOverrides temporarily applies per-request routing-mode,
// safety-level, agentic-loop, and tool-access overrides to the LIVE router,
// permission gate, and kernel flags (the objects Execute actually consults),
// and returns a restore function that must be deferred to revert to the
// previous state.
//
// Empty strings leave the corresponding setting unchanged. `loop` is "on"
// (force the ReAct loop), "off" (force it off), or "" (master toggle). `tools`
// is "off" (disable tool access — General mode fast path) or ""/"on" (tools
// enabled). This makes the `mode`/`safety`/`chat_mode` fields of POST
// /api/chat take effect for that single request.
func (k *Kernel) ApplyRequestOverrides(mode, safety, loop, tools string) func() {
	var oldMode core.RoutingMode
	var oldLevel permission.Level
	var oldReqLoop *bool
	var oldToolsDisabled *bool
	haveMode := mode != ""
	haveSafety := safety != ""
	haveLoop := loop != ""
	haveTools := tools != ""
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
			switch strings.ToLower(tools) {
			case "off":
				disabled := true
				k.requestToolsDisabled = &disabled
			case "on":
				enabled := false
				k.requestToolsDisabled = &enabled
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
		if haveLoop || haveTools {
			k.mu.Lock()
			if haveLoop {
				k.requestLoop = oldReqLoop
			}
			if haveTools {
				k.requestToolsDisabled = oldToolsDisabled
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

// CompressProjectContext produces a concise narrative briefing of a project's
// raw context.md using the configured compressor model (the same model used
// for STM compression). This is the project-level counterpart to the
// per-conversation Compress call: it persists a compact summary so that when
// a project is reopened after a long gap, the LLM receives a short advance
// briefing instead of up to 1 MiB of raw session log.
//
// It is a thin, nil-safe wrapper over compression.Compressor.Summarize so the
// server (which holds the kernel, not the compressor) can request project
// compression without reaching into compression internals, and so that ONLY
// the compressor model performs context compression (STM + project).
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
// EXECUTION LOOP: Observe → Compress → Plan → Route → Execute → Validate → Merge → Store
// ============================================================================

// Execute is the main entry point. It runs the full execution loop.
// maxPlanInjectBytes caps how much of the plan/workflow markdown is
// injected into the goal, so a very long plan can't blow out the context
// window. The plan is truncated with a marker when it exceeds this.
const maxPlanInjectBytes = 8192

// SetProjectContext stashes the active project's implementation plan and
// workflow architecture so Execute can inject them into the goal for the
// current request. Call ClearProjectContext (usually via defer) afterwards.
// Safe to call with empty strings (no-op injection). The kernel deliberately
// takes raw strings (not a *project.Store) to avoid coupling the orchestrator
// to the project package.
func (k *Kernel) SetProjectContext(plan, workflow string) {
	k.mu.Lock()
	k.projectPlan = plan
	k.projectWorkflow = workflow
	k.mu.Unlock()
}

// ClearProjectContext resets the per-request plan/workflow stash. Call after
// Execute (typically deferred) so a subsequent non-project request isn't
// contaminated with the previous project's plan.
func (k *Kernel) ClearProjectContext() {
	k.mu.Lock()
	k.projectPlan = ""
	k.projectWorkflow = ""
	k.mu.Unlock()
}

// injectProjectContext prepends the stashed plan/workflow (if any) to the
// goal so every execution path (general, loop, trivial-direct, DAG) follows
// the active project's plan. No-op when no project is active.
func (k *Kernel) injectProjectContext(goal string) string {
	k.mu.Lock()
	plan := k.projectPlan
	workflow := k.projectWorkflow
	k.mu.Unlock()
	plan = strings.TrimSpace(plan)
	workflow = strings.TrimSpace(workflow)
	if plan == "" && workflow == "" {
		return goal
	}
	var sb strings.Builder
	sb.WriteString("IMPORTANT EXECUTION DIRECTIVE:\nAll your implementations, tool calls, and responses MUST strictly adhere to the provided Implementation Plan, Architecture, and Task Workflow below. You are not allowed to deviate from these documents.\n\n")
	if plan != "" {
		sb.WriteString("## Implementation Plan & Architecture\n")
		sb.WriteString(truncateMid(plan, maxPlanInjectBytes))
		sb.WriteString("\n\n")
	}
	if workflow != "" {
		sb.WriteString("## Task Workflow\n")
		sb.WriteString(truncateMid(workflow, maxPlanInjectBytes))
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Task Goal\n")
	sb.WriteString(goal)
	return sb.String()
}

// truncateMid returns s if it fits within maxBytes, otherwise a prefix +
// truncation marker + suffix so the beginning and end of the plan are both
// visible (the end is often the most current/important part).
func truncateMid(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	head := maxBytes * 3 / 4
	tail := maxBytes - head - len("\n…[truncated]…\n")
	if tail < 0 {
		tail = 0
	}
	return s[:head] + "\n…[truncated]…\n" + s[len(s)-tail:]
}

func (k *Kernel) Execute(ctx context.Context, userGoal string) (string, error) {
	k.log("observe", "Processing user goal: "+userGoal)

	// Step 1: Observe — add to STM
	k.memory.STMAdd(core.Message{
		Role:    core.RoleUser,
		Content: userGoal,
	})

	// Step 1.5: Inject the active project's implementation plan + workflow
	// architecture (if any) into the goal so every downstream path (general,
	// loop, trivial-direct, DAG) FOLLOWS the plan rather than decomposing from
	// the goal alone. Previously the plan/workflow was a write-only display
	// artifact generated after execution; now it drives implementation.
	userGoal = k.injectProjectContext(userGoal)

	// Step 2: Compress context (if enabled and history is long enough to be
	// worth an LLM call). Running compression on every request — including a
	// trivial single-turn "what is 2+2" — adds latency and cost for no benefit,
	// so we skip it until the STM window has accumulated meaningful history.
	if k.cfg.CompressContext && k.compressor != nil {
		stm := k.memory.STMGet()
		// Only compress when there's enough history AND enough new growth
		// since the last compression — avoids re-compressing the same window
		// twice if two requests land while STM is between thresholds.
		if len(stm) >= compressionMinHistory && len(stm)-k.lastCompressedLen >= compressionMinGrowth {
			k.log("compress", "Compressing context")
			snapshot, err := k.compressor.Compress(ctx, stm, userGoal)
			if err == nil && snapshot != nil {
				k.log("compress", fmt.Sprintf("Context compressed: %d→%d tokens (ratio: %.1f%%)",
					snapshot.OriginalTokens, snapshot.CompressedTokens,
					snapshot.CompressionRatio()))
				if k.emitter != nil {
					k.emitter.EmitCompression(snapshot.OriginalTokens, snapshot.CompressedTokens)
				}
				// Inject the snapshot: replace older STM with the compressed
				// briefing + a recent tail. This makes compression actually
				// shrink the conversation for ALL modes (general, project,
				// loop) — previously SnapshotToMessages was never called, so
				// the snapshot was discarded and compression cost an LLM call
				// for nothing. The compressed briefing IS the general-mode
				// context: a durable summary that survives across the trim
				// boundary instead of hard-dropping old messages.
				briefing := compression.SnapshotToMessages(snapshot)
				k.memory.STMCompress(briefing, compressionKeepRecent)
				k.lastCompressedLen = len(k.memory.STMGet())
			}
		}
	}

	// Step 3: Assess complexity
	complexity := router.AssessComplexity(userGoal)
	k.log("plan", fmt.Sprintf("Task complexity: %d/10", complexity))

	// Step 3.01: Fetch Hybrid Recall (Memory + KG) so all paths can use it
	recallBlock := k.getRecallBlock(userGoal)

	// Step 3.02: Confident Recall (Semantic Cache, graph-first LLM skip)
	// Try Confident Recall first: an exact (normalized) match, or a strict
	// near-duplicate match, against a prior successful no-tool task. If it
	// hits, we return the cached answer immediately without calling the LLM
	// at all. This applies globally (including smart mode) because
	// ConfidentRecall safely filters out any past tool-using tasks and only
	// matches at a conservative similarity threshold — see its doc comment.
	if k.retriever != nil {
		if ans, ok := k.retriever.ConfidentRecall(userGoal, 0); ok {
			k.log("memory", "Confident recall matched! Returning cached response without LLM call.")
			k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: ans})
			if k.emitter != nil {
				k.emitter.EmitFinalOutput(ans)
			}
			return ans, nil
		}
	}

	// Step 3.03: Clarification gate — some requests are too vague to act on
	// safely. Ask for more detail instead of burning turns on speculative
	// planning or tool use.
	if needsClarification(userGoal, complexity) {
		k.log("plan", "Request is ambiguous — requesting clarification")
		clarification := "I can help, but I need a bit more detail to act safely and accurately. Please specify the target file, expected behavior, and any constraints or examples."
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: clarification})
		k.storeEpisodic(userGoal, clarification, nil, false, recallBlock)
		k.recordLearningAndAudit(userGoal, clarification, nil, false, "clarification", 0)
		if k.emitter != nil {
			k.emitter.EmitFinalOutput(clarification)
		}
		return clarification, nil
	}

	// Step 3.05: General mode fast path — when tool access is disabled for
	// this request (chat_mode=="general"), take a lightweight single-LLM-call
	// path with NO tools offered. This is pure conversation: no DAG, no worker
	// agents, no approval popups, no tool overhead. It is intentionally taken
	// BEFORE the loop/DAG/consensus-trivial branches so General mode never
	// offers tools even if the master loop toggle is on or consensus is set.
	if k.toolsDisabledForRequest() {
		k.log("plan", "General mode (tools disabled) — direct conversational path")
		return k.executeDirectNoTools(ctx, userGoal, recallBlock)
	}

	// Step 3.5: Agentic Loop (looping technology) — optional ReAct execution.
	// When enabled (per-request Loop mode in the web chat, or the master
	// toggle for the CLI), delegate the whole task to the Sense-Think-Act loop
	// (which uses tools) and skip the DAG decomposition. After the loop
	// produces a final answer, if consensus mode is on and multiple models are
	// registered, run a consensus synthesis round so non-primary models
	// review/enhance the answer and the primary synthesizes the final output.
	// The synthesis is GROUNDED in the loop's tool trace so refiners cannot
	// hallucinate that the agent lacks tool access — the previous design ran
	// consensus (text-only) without the trace, so a "skeptic" model overrode
	// real tool output with "I cannot create files".
	if k.loopEnabledForRequest() && k.agenticLoop != nil {
		k.log("loop", "Agentic loop (ReAct) enabled — running Sense-Think-Act cycle")
		loopRes, err := k.agenticLoop.Run(ctx, k.injectRecall(userGoal, recallBlock))
		if err != nil {
			k.storeEpisodic(userGoal, "", nil, false, recallBlock)
			return "", err
		}
		output := loopRes.Output

		// Post-loop consensus synthesis: non-primary models review the agentic
		// loop's answer from their role perspectives; primary synthesizes. Tools
		// already executed during the loop — this just refines the final answer.
		// The tool trace is passed in so the reviewers know the tools ran for
		// real and cannot claim the agent cannot take action.
		if k.router.GetMode() == core.RouteConsensus && k.router.ModelCount() > 1 {
			k.log("consensus", "Running post-agentic consensus synthesis")
			if refined, cerr := k.runConsensusOnOutput(ctx, userGoal, output, loopRes.ToolTrace); cerr == nil {
				output = refined
			} else {
				k.log("consensus", "Consensus synthesis failed: "+cerr.Error()+" — using agentic output")
			}
		}

		// Record to STM + episodic/learning/audit/KG so the rest of the system
		// sees the task just like a DAG execution.
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
		k.storeEpisodic(userGoal, output, nil, true, recallBlock)
		k.recordLearningAndAudit(userGoal, output, nil, true, "agentic-loop", 0)
		if k.emitter != nil {
			k.emitter.EmitFinalOutput(output)
		}
		return output, nil
	}

	// Step 3.6 (REMOVED): Previously, a trivial knowledge question in consensus
	// mode was intercepted here by a text-only runConsensus() round with NO
	// tools offered. That intercepted Project/Auto mode requests like "create a
	// file" (classified trivial) before they reached the tool-capable path, so
	// tools were silently unavailable even though those modes advertise tools.
	// Tool-disabled (General) mode is already handled at Step 3.05 above
	// (executeDirectNoTools, which itself runs consensus text-only), so this
	// branch was both redundant for General and harmful for Project/Auto. It is
	// removed so every tool-enabled mode reaches executeDirect (trivial) or the
	// DAG (non-trivial), both of which offer tools. Consensus is still honored:
	// the DAG merges via mergeWithConsensus (all models) and the loop runs a
	// grounded consensus synthesis round.

	// Step 4: Decide — trivial task or needs decomposition?
	if k.isTrivial(userGoal, complexity) {
		k.log("plan", "Task is trivial — executing directly")
		return k.executeDirect(ctx, userGoal, recallBlock)
	}

	// Step 5: Plan — decompose into DAG using planner agent.
	// We inject the context into the planner's prompt, but leave the raw userGoal
	// unmodified for episodic memory storage.
	k.log("plan", "Decomposing task into DAG")
	d, err := k.planAndDecompose(ctx, k.injectRecall(userGoal, recallBlock))
	if err != nil {
		// Fallback: execute directly if planning fails
		k.log("plan", "Planning failed: "+err.Error()+" — falling back to direct execution")
		return k.executeDirect(ctx, userGoal, recallBlock)
	}

	if d == nil || d.IsEmpty() {
		k.log("plan", "No tasks in DAG — executing directly")
		return k.executeDirect(ctx, userGoal, recallBlock)
	}

	k.log("plan", fmt.Sprintf("DAG has %d tasks", d.NodeCount()))

	// Step 6: Execute the DAG
	k.log("execute", "Executing task DAG")
	results, err := k.executeDAG(ctx, d, userGoal)
	if err != nil {
		if len(results) == 0 {
			return "", fmt.Errorf("DAG execution failed: %w", err)
		}
		// Best-effort recovery: a deadlock or cancellation aborted the DAG,
		// but some sub-agents did complete. Synthesize what we have instead
		// of discarding all completed work.
		k.log("execute", fmt.Sprintf("DAG execution failed (%v) — merging %d completed sub-task result(s)", err, len(results)))
		merged, mergeErr := k.mergeResults(ctx, results, userGoal)
		if mergeErr != nil {
			return "", fmt.Errorf("DAG execution failed: %w", err)
		}
		merged = fmt.Sprintf("[Partial result — DAG execution did not complete: %v]\n\n%s", err, merged)
		k.storeEpisodic(userGoal, merged, results, false, recallBlock)
		k.recordLearningAndAudit(userGoal, merged, results, false, "dag", 0)
		return merged, nil
	}

	// Step 7: Merge results
	k.log("merge", "Merging sub-agent results")
	merged, err := k.mergeResults(ctx, results, userGoal)
	if err != nil {
		return "", fmt.Errorf("merge failed: %w", err)
	}

	// Step 7.5: Verify output (self-verification pipeline)
	if k.verifier != nil {
		k.log("verify", "Running self-verification pipeline")
		var toolNames []string
		for _, r := range results {
			for _, tc := range r.ToolCalls {
				toolNames = append(toolNames, tc.Function.Name)
			}
		}
		vResult, vErr := k.verifier.QuickVerify(ctx, userGoal, merged)
		if vErr == nil && vResult != nil {
			k.log("verify", fmt.Sprintf("Confidence: %.2f (threshold: %.2f, passed: %v)",
				vResult.Confidence.Overall, 0.6, vResult.Passed))
		}
	}

	// Step 8: Store episodic memory
	k.log("store", "Storing episodic memory")
	k.storeEpisodic(userGoal, merged, results, true, recallBlock)

	// Step 8.5 + 8.6: Record learning feedback + audit + knowledge graph.
	// Step 9 (self-improvement / skill extraction) is folded into this call
	// via minSkillSuccess=2 — see recordLearningAndAudit's doc comment.
	k.log("improve", "Recording learning feedback and extracting reusable skills")
	k.recordLearningAndAudit(userGoal, merged, results, true, "dag", 2)

	// Step 10: Emit final output
	if k.emitter != nil {
		k.emitter.EmitFinalOutput(merged)
	}

	k.memory.STMAdd(core.Message{
		Role:    core.RoleAssistant,
		Content: merged,
	})

	return merged, nil
}

// ============================================================================
// TRIVIAL TASK DETECTION
// ============================================================================

func (k *Kernel) isTrivial(goal string, complexity int) bool {
	// Trivial = low complexity AND short prompt AND no multi-step indicators.
	// Ambiguous requests are handled before this branch, so they do not get
	// routed into speculative tool execution.
	if complexity > 5 {
		return false
	}
	if len(goal) > 300 {
		return false
	}

	multiStepIndicators := []string{
		" and then ", " after that ", " step by step",
		" first ", " second ", " third ",
		" multiple ", " simultaneously", " in parallel",
		" decompose", " break down", " plan",
	}
	goalLower := strings.ToLower(goal)
	for _, indicator := range multiStepIndicators {
		if strings.Contains(goalLower, indicator) {
			return false
		}
	}

	return true
}

func (k *Kernel) resolveSequential() bool {
	return k.cfg.MaxConcurrent <= 1
}

func needsClarification(goal string, complexity int) bool {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return true
	}
	goalLower := strings.ToLower(goal)
	if len(goalLower) < 8 {
		return true
	}
	if complexity >= 5 {
		return false
	}
	vaguePhrases := []string{
		"fix it",
		"make it better",
		"improve this",
		"do something",
		"help me",
		"work on it",
		"handle it",
		"update it",
		"make it work",
	}
	for _, phrase := range vaguePhrases {
		if strings.Contains(goalLower, phrase) {
			return true
		}
	}
	concreteIndicators := []string{"file", "function", "bug", "error", "implement", "add", "create", "debug", "refactor", "test", "route", "endpoint", "json", "go", "python", "sql", "class"}
	for _, indicator := range concreteIndicators {
		if strings.Contains(goalLower, indicator) {
			return false
		}
	}
	return true
}

func (k *Kernel) executeDirect(ctx context.Context, goal string, recallBlock string) (string, error) {
	// Use a single worker agent
	cfg := core.SubAgentConfig{
		Role:      core.RoleWorker,
		Goal:      k.injectRecall(goal, recallBlock),
		ModelTier: core.ModelTierCoding,
		MaxTurns:  k.cfg.MaxTurns,
	}

	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("direct", "planning", "Executing trivial task directly")
	}

	agent, err := k.factory.Spawn(ctx, cfg)
	if err != nil {
		return "", err
	}

	result, err := agent.Execute(ctx)
	if err != nil {
		k.storeEpisodic(goal, "", []*core.SubAgentResult{result}, false, recallBlock)
		return "", err
	}

	if k.emitter != nil {
		k.emitter.EmitFinalOutput(result.Output)
	}

	k.storeEpisodic(goal, result.Output, []*core.SubAgentResult{result}, true, recallBlock)
	// Direct tasks that actually used tools can still yield a simple skill —
	// minSkillSuccess=1 folds that in, see recordLearningAndAudit.
	k.recordLearningAndAudit(goal, result.Output, []*core.SubAgentResult{result}, true, "direct", 1)
	return result.Output, nil
}

// generalModeNoToolsPrompt is the shared "no tools available" system prompt
// for General mode, used by both the single-model path and the consensus
// fan-out path below — previously duplicated with slightly different wording
// in each place.
const generalModeNoToolsPrompt = "You are DarkCode in General (conversational) mode. Answer the user directly and helpfully. " +
	"You do NOT have access to any tools in this mode — you cannot create, read, or modify files, run terminal " +
	"commands, or perform any real-world action. Never claim an action was completed or output a shell command as " +
	"if it executed. If the task requires creating files, running commands, or other real actions, tell the user " +
	"to switch to Project, Auto, or Loop mode."

// executeDirectNoTools is the General-mode fast path: a single LLM call with
// NO tools offered. It is pure conversation — no DAG, no worker agents, no
// approval prompts, no tool overhead. Used when toolsDisabledForRequest() is
// true (chat_mode=="general"). It still participates in consensus when
// multiple models are registered (both models answer, primary synthesizes)
// because that is a text-only refinement and never touches tools.
func (k *Kernel) executeDirectNoTools(ctx context.Context, goal string, recallBlock string) (string, error) {
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("general", "planning", "Conversational response (no tools)")
	}

	// Consensus path (text-only): if multiple models are registered and
	// consensus mode is on, fan out to both models and synthesize. This never
	// offers tools, so it cannot accidentally execute anything.
	if k.router.GetMode() == core.RouteConsensus && k.router.ModelCount() > 1 {
		k.log("consensus", "Running multi-model consensus (General mode, no tools)")
		output, err := k.runConsensus(ctx, goal, generalModeNoToolsPrompt)
		if err == nil {
			k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
			k.storeEpisodic(goal, output, nil, true, recallBlock)
			k.recordLearningAndAudit(goal, output, nil, true, "general-consensus", 0)
			if k.emitter != nil {
				k.emitter.EmitFinalOutput(output)
			}
			return output, nil
		}
		k.log("consensus", "Consensus failed: "+err.Error()+" — falling back to single-model")
	}

	// Single-model path: route to the coding tier and call with NO tools.
	complexity := router.AssessComplexity(goal)
	client, modelName, err := k.router.Route(core.ModelTierCoding, complexity, goal)
	if err != nil {
		return "", fmt.Errorf("general mode: model routing failed: %w", err)
	}

	stm := k.memory.STMGet()

	sysContent := generalModeNoToolsPrompt

	if recallBlock != "" {
		sysContent += "\n\n## Relevant Past Context\n" + recallBlock
	}

	// When UseCtxEngine is enabled, assemble a deduplicated, budget-trimmed
	// context window instead of dumping raw STM (Strategy 6b). Disabled by
	// default and falls back to the original raw-append behavior on any
	// error, so this is strictly opt-in.
	var messages []core.Message
	if engine := k.getCtxEngine(); engine != nil {
		window, err := engine.Assemble(ctx, ctxengine.AssembleRequest{
			Query:           goal,
			Conversation:    stm,
			SystemPrompt:    sysContent,
			AvailableTokens: client.ModelInfo().Context,
		})
		if err == nil && window != nil {
			messages = window.Messages
		}
	}
	if messages == nil {
		messages = make([]core.Message, 0, len(stm)+1)
		messages = append(messages, core.Message{Role: core.RoleSystem, Content: sysContent})
		messages = append(messages, stm...)
	}

	temp := 0.7
	req := &llm.CompletionRequest{
		Model:       modelName,
		Messages:    messages,
		Temperature: &temp,
		// Deliberately NO Tools field — General mode is tool-free.
	}

	resp, err := client.ChatCompletionStream(ctx, req, &llm.StreamCallbacks{
		OnContent: func(chunk string) {
			if k.emitter != nil {
				k.emitter.Emit(core.EventTaskUpdate, chunk,
					ui.WithTaskID("general"), ui.WithStatus("streaming"))
			}
		},
	})
	if err != nil {
		k.storeEpisodic(goal, "", nil, false, recallBlock)
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("general mode: empty response")
	}
	output := resp.Choices[0].Message.Content

	k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
	k.storeEpisodic(goal, output, nil, true, recallBlock)
	k.recordLearningAndAudit(goal, output, nil, true, "general", 0)
	if k.emitter != nil {
		k.emitter.EmitFinalOutput(output)
	}
	return output, nil
}



// semanticKey produces a stable, filesystem/JSON-safe key from a goal string.
func semanticKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	for _, ch := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r"} {
		s = strings.ReplaceAll(s, ch, "-")
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// extractPaths finds plausible file paths in the goal + output text.
func extractPaths(texts ...string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, text := range texts {
		for _, tok := range strings.Fields(text) {
			tok = strings.Trim(tok, "\"'`,.;()[]{}")
			if len(tok) < 3 || len(tok) > 256 {
				continue
			}
			// Absolute paths or paths with a slash and an extension.
			if (strings.HasPrefix(tok, "/") || strings.Contains(tok, "/")) && strings.Contains(tok, ".") {
				if !seen[tok] {
					seen[tok] = true
					paths = append(paths, tok)
				}
			}
		}
		if len(paths) >= 20 {
			break
		}
	}
	return paths
}

func truncateID(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func truncateLabel(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func generateSkillName(goal string) string {
	// Create a snake_case name from the goal. Strip punctuation by chaining
	// replacements on an accumulator — earlier code reassigned from the loop
	// variable `w` on each line, which discarded every replacement but the
	// last (commas and dots survived in skill names).
	words := strings.Fields(strings.ToLower(goal))
	if len(words) > 5 {
		words = words[:5]
	}
	for i, w := range words {
		clean := w
		for _, ch := range []string{",", ".", ":", ";", "!", "?"} {
			clean = strings.ReplaceAll(clean, ch, "")
		}
		words[i] = clean
	}
	return "skill_" + strings.Join(words, "_")
}

// pluralY returns "y"/"ies" for 1/non-1.
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

type RouterModelInfo struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	Tier      string `json:"tier"`
	Endpoints int    `json:"endpoints"`
	IsPrimary bool   `json:"is_primary"`
}

func (k *Kernel) SequentialMode() bool {
	return k.cfg.MaxConcurrent <= 1
}

func (k *Kernel) SetExecutionProfile(profile string) {
	k.cfg.ExecutionProfile = profile
	if profile == "sequential" {
		k.cfg.MaxConcurrent = 1
	} else if profile == "parallel" {
		k.cfg.MaxConcurrent = 10
	}
}

func (k *Kernel) SetModelRole(modelName, role string) {
	k.router.SetModelRole(modelName, role)
}

func (k *Kernel) RegisteredModels() []RouterModelInfo {
	var info []RouterModelInfo
	// Pull from router
	if k.router != nil {
		for _, m := range k.router.AllModels() {
			info = append(info, RouterModelInfo{
				Name:      m.Name,
				Role:      m.Role,
				Tier:      string(m.Tier),
				IsPrimary: m.Role == "synthesizer" || m.Role == "primary", // Basic heuristic
			})
		}
	}
	return info
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
