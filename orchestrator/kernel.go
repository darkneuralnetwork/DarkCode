package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/agents"
	"github.com/darkcode/checkpoint"
	"github.com/darkcode/compression"
	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/ctxengine"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/llm"
	"github.com/darkcode/loop"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/permission"
	"github.com/darkcode/plan"
	"github.com/darkcode/router"
	"github.com/darkcode/safeurl"
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

	// checkpoints snapshots the workspace before each mutating tool so the
	// user can undo the agent. nil when the store could not be opened.
	checkpoints *checkpoint.Manager

	// runsDir holds per-run execution journals, which make a DAG resumable
	// after a crash and replayable for a post-mortem. Empty disables both.
	runsDir string

	// modeApprover is the single mode-aware approval router installed on the
	// gate. It delegates each prompt to the GUI ServerApprover or the CLI
	// terminal approver based on the active UI mode, so switching surfaces
	// never leaves a stale approver (the prior CLI→GUI permission bug).
	modeApprover *permission.ModeAwareApprover

	// agenticLoop is the optional ReAct execution loop (looping technology).
	// Non-nil always; whether it runs is decided per request — the loop, tool
	// scope, read-only and planning overrides all live on the request's
	// context now, not here. See request_state.go for why.
	agenticLoop *loop.ReActLoop

	// reviewerOn gates the post-acceptance reviewer. Off by default: an extra
	// model call on every successful run is a real cost for advice nobody
	// asked for, and it can never change the outcome. See reviewer.go.
	reviewerOn bool
	// debateOn gates the conflict-triggered exchange. Off by default: it only
	// fires when models disagree AND the graph cannot check them, but that is
	// still two extra calls on a metered tier. See debate.go.
	debateOn bool

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

	// Scoped-override bookkeeping. A verb applies to one message, and the
	// mechanism is save-old / set-new / restore-on-return — but the thing
	// saved and restored is shared router and gate state, so two overlapping
	// requests interleaved their saves: the second captured the first's
	// override as its "base" and put it back on the way out, leaving the
	// router in a mode no request had asked for. Stuck in consensus means
	// every later query fans out to every registered model.
	//
	// Counting concurrent overrides fixes that: the first captures the real
	// base, the last restores it, and whatever happens in between is
	// transient rather than permanent.
	overrideMu    sync.Mutex
	modeDepth     int
	modeBase      core.RoutingMode
	safetyDepth   int
	safetyBase    permission.Level
	forceLocDepth int
	forceLocBase  bool

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
	loop := k.agenticLoop
	k.mu.Unlock()

	// The loop consults the same governor between acting turns. Checking once
	// before the request started meant a cap could be reached on iteration two
	// and the remaining twenty-three still ran.
	if loop == nil {
		return
	}
	if g == nil {
		loop.SetBudgetCheck(nil)
		return
	}
	loop.SetBudgetCheck(func() error {
		if d := g.Check(); !d.Allowed {
			return fmt.Errorf("cost limit reached: %s", d.Reason)
		}
		return nil
	})
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
		agenticLoop: loop.New(rtr, reg, emitter, 0), // 0 → loop.DefaultMaxLoops
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

// newConfiguredClient builds an LLM client from a model config, applying the
// provider, the credential pool, and the reasoning effort. Shared by every
// registration site here so a model reloaded from the GUI gets the same
// treatment as one wired at startup.
func newConfiguredClient(mc config.ModelConfig) *llm.Client {
	c := llm.NewClient(mc.BaseURL, mc.APIKey, mc.Model)
	c.SetProvider(mc.Provider)
	c.Keys = llm.NewKeyPool(append([]string{mc.APIKey}, mc.APIKeys...)...)
	c.Effort = mc.ReasoningEffort
	return c
}

// SetCheckpoints installs the workspace snapshotter, both on the tool registry
// (which takes the pre-mutation snapshot) and on the kernel, so every surface
// can offer rollback from one owner.
func (k *Kernel) SetCheckpoints(m *checkpoint.Manager) {
	k.checkpoints = m
	if k.registry != nil {
		k.registry.SetCheckpointer(m)
	}
}

// Checkpoints returns the workspace snapshotter, or nil when unavailable.
func (k *Kernel) Checkpoints() *checkpoint.Manager { return k.checkpoints }

// SetRunsDir enables durable, resumable DAG execution by journalling each run
// under dir.
func (k *Kernel) SetRunsDir(dir string) { k.runsDir = dir }

// RunEvents returns the recorded execution events for a goal's run, oldest
// first — the raw material for a replay view or a post-mortem.
func (k *Kernel) RunEvents(goal string) []ExecEvent {
	// Read-only: NewExecJournal would delete a finished run's journal as part
	// of preparing to re-run it.
	return ReadRunEvents(k.runsDir, goal)
}

// RollbackTo restores the workspace to checkpoint n and rewinds the transcript
// to match, so the agent's next turn reasons from the state on disk. Shared by
// the CLI /rollback command and the HTTP API.
func (k *Kernel) RollbackTo(n int) (checkpoint.Entry, []checkpoint.Change, error) {
	if k.checkpoints == nil {
		return checkpoint.Entry{}, nil, fmt.Errorf("checkpoints are not enabled")
	}
	entry, changes, err := k.checkpoints.Rollback(n)
	if err != nil {
		return checkpoint.Entry{}, nil, err
	}
	k.memory.STMTruncate(entry.Turn)
	k.softenBeliefsAfterRollback(changes)
	return entry, changes, nil
}

// rollbackConfidenceDecay is how much a rolled-back file's beliefs lose, and
// how fast that loss fades across the graph. Values are deliberately modest:
// the facts are suspect, not refuted, and re-indexing restores them.
const (
	rollbackConfidenceDecay = -0.2
	rollbackDecayFactor     = 0.5
	rollbackDecayHops       = 2
)

// softenBeliefsAfterRollback lowers confidence in what the graph believes about
// the files a rollback just rewrote, and — with decay — about their neighbours.
//
// A rollback is evidence: the code on disk is no longer what the graph was
// indexed against, so every fact derived from those files is now questionable,
// and facts about the code that references them slightly less so. Leaving the
// graph at full confidence would have the agent citing beliefs about a file
// that has since been reverted underneath it.
func (k *Kernel) softenBeliefsAfterRollback(changes []checkpoint.Change) {
	kg, ok := k.memory.KG().(*memory.KnowledgeGraph)
	if !ok || kg == nil || len(changes) == 0 {
		return
	}
	softened := 0
	for _, c := range changes {
		softened += kg.PropagateConfidence("file:"+c.Path,
			rollbackConfidenceDecay, rollbackDecayFactor, rollbackDecayHops)
	}
	if softened > 0 {
		k.log("memory", fmt.Sprintf(
			"Rollback: lowered confidence in %d belief(s) derived from the reverted files", softened))
	}
}

// ApplyRequestOverrides applies per-request routing/safety/loop/tool overrides
// and returns the context the request must execute under, plus a restore func
// to defer. Empty strings leave a setting unchanged; loop is "on"/"off"/"",
// tools is "off"/"readonly"/"on". Lets the mode/safety/chat_mode fields of
// POST /api/chat apply for a single request.
//
// The returned context carries the loop/tools/read-only decisions. Those used
// to be written to fields on the shared Kernel, which meant two overlapping
// requests fought over one field and the earlier one finished under the later
// one's settings — see request_state.go. Callers MUST pass the returned
// context to Execute; passing the original silently restores the old bug.
//
// Mode, safety and brain still mutate shared router/gate state under a depth
// counter, because a request does not own those objects.
func (k *Kernel) ApplyRequestOverrides(ctx context.Context, mode, safety, loop, tools, brain string) (context.Context, func()) {
	var oldMode core.RoutingMode
	var oldLevel permission.Level
	var oldForceLocal *bool
	haveMode := mode != ""
	haveSafety := safety != ""
	haveLoop := loop != ""
	haveTools := tools != ""

	rs := &requestState{}
	// Brain selector (per-request): "local" pins to the local model (offline),
	// "cloud" allows cloud routing, "auto"/"" leave the configured default. Only
	// local/cloud actually change the router's force-local flag.
	haveBrain := false
	if k.router != nil {
		switch strings.ToLower(brain) {
		case "local", "cloud":
			k.overrideMu.Lock()
			if k.forceLocDepth == 0 {
				k.forceLocBase = k.router.ForceLocal()
			}
			k.forceLocDepth++
			cur := k.forceLocBase
			k.overrideMu.Unlock()
			oldForceLocal = &cur
			k.router.SetForceLocal(strings.ToLower(brain) == "local")
			haveBrain = true
		}
	}
	if haveMode && k.router != nil {
		k.overrideMu.Lock()
		if k.modeDepth == 0 {
			k.modeBase = k.router.GetMode()
		}
		k.modeDepth++
		oldMode = k.modeBase
		k.overrideMu.Unlock()
		k.router.SetMode(parseRoutingModeLocal(mode))
	}
	if haveSafety && k.gate != nil {
		k.overrideMu.Lock()
		if k.safetyDepth == 0 {
			k.safetyBase = k.gate.Level()
		}
		k.safetyDepth++
		oldLevel = k.safetyBase
		k.overrideMu.Unlock()
		k.gate.SetLevel(permission.LevelFromString(safety))
	}
	// The per-request half. No lock and no saved "old" value: these go into the
	// request's own context, so there is nothing shared to protect or restore.
	if haveLoop {
		switch strings.ToLower(loop) {
		case "on":
			on := true
			rs.loop = &on
		case "off":
			off := false
			rs.loop = &off
		}
	}
	if haveTools {
		disabled, enabled, readOnly := true, false, true
		switch strings.ToLower(tools) {
		case "off":
			rs.toolsDisabled = &disabled
		case "readonly", "read-only":
			// Chat mode: tools enabled, but only read-only ones offered.
			rs.toolsDisabled = &enabled
			rs.readOnly = &readOnly
		case "on":
			rs.toolsDisabled = &enabled
		}
	}
	if haveLoop || haveTools {
		ctx = withRequestState(ctx, rs)
	} else if ctx == nil {
		ctx = context.Background()
	}
	return ctx, func() {
		if haveMode && k.router != nil {
			k.overrideMu.Lock()
			k.modeDepth--
			last := k.modeDepth == 0
			k.overrideMu.Unlock()
			if last {
				k.router.SetMode(oldMode)
			}
		}
		if haveSafety && k.gate != nil {
			k.overrideMu.Lock()
			k.safetyDepth--
			last := k.safetyDepth == 0
			k.overrideMu.Unlock()
			if last {
				k.gate.SetLevel(oldLevel)
			}
		}
		if haveBrain && k.router != nil && oldForceLocal != nil {
			k.overrideMu.Lock()
			k.forceLocDepth--
			last := k.forceLocDepth == 0
			k.overrideMu.Unlock()
			if last {
				k.router.SetForceLocal(*oldForceLocal)
			}
		}
		// loop/tools need no restore: they never left this request's context.
	}
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
		client := newConfiguredClient(mc)
		k.router.RegisterModel(modelTierFromString(mc.Tier), client, mc.Model)
		// Set the consensus role from config (empty = default "general").
		k.router.SetModelRole(mc.Model, mc.Role)
	}

	// Register the primary model at the mode-appropriate tier (reasoning for
	// consensus, coding otherwise). This ensures the primary wins its tier
	// slot for Route/escalation, and MarkPrimary below flags it as the
	// consensus synthesizer.
	primaryClient := newConfiguredClient(config.ModelConfig{
		Provider: cfg.Provider, BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model})
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
				compClient = newConfiguredClient(mc)
				compModel = mc.Model
			}
		}
		k.compressor.SetClient(compClient, compModel)
		k.log("compression", "Context compressor model: "+compModel+
			" (user-selected="+strutil.NonEmpty(cfg.CompressorModel, "<primary>")+")")
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
	// Air-gap and per-model reliability are states the user has no other way
	// to confirm — a silently-inactive air gap is exactly the kind of thing
	// someone would rather find out here than after the fact.
	egress := "on (outbound network allowed)"
	if safeurl.AirGapped() {
		egress = "OFF — air-gapped, no outbound network"
	}
	var reliability string
	for _, w := range k.router.Reliability() {
		if w.TotalCalls >= 3 {
			reliability += fmt.Sprintf("    %s as %s: %.0f%% over %d call(s)\n",
				w.ModelName, w.Role, w.SuccessRate*100, w.TotalCalls)
		}
	}
	if reliability == "" {
		reliability = "    (not enough calls recorded yet)\n"
	}

	return fmt.Sprintf(
		"Orchestrator Kernel:\n"+
			"  Routing mode: %s\n"+
			"  UI mode: %v\n"+
			"  Safety level: %d\n"+
			"  Max concurrent: %d\n"+
			"  Compress context: %v\n"+
			"  Task log entries: %d\n"+
			"  Egress: %s\n"+
			"  Model reliability:\n%s"+
			"\n%s",
		k.cfg.RoutingMode,
		k.cfg.UIMode,
		k.cfg.SafetyLevel,
		k.cfg.MaxConcurrent,
		k.cfg.CompressContext,
		len(k.taskLog),
		egress,
		reliability,
		k.memory.Summary(),
	)
}

// Runs lists the recorded execution journals, most recent first, so a replay
// view can offer something to open.
func (k *Kernel) Runs() []RunSummary { return ListRuns(k.runsDir) }
