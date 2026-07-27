package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/darkcode/candidate"
	"github.com/darkcode/capability"
	"github.com/darkcode/checkpoint"
	"github.com/darkcode/compression"
	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/ingest"
	"github.com/darkcode/llm"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/observability"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/plugin"
	"github.com/darkcode/project"
	"github.com/darkcode/provider"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/router"
	"github.com/darkcode/safeurl"
	"github.com/darkcode/security"
	"github.com/darkcode/server"
	"github.com/darkcode/tools"
	"github.com/darkcode/tools/deterministic"
	"github.com/darkcode/ui"
)

func (a *AppRunner) WireUp() {
	a.initObservabilityAndSecurity()
	memDir := a.initMemoryAndProjects()
	// Router must exist before tools: RegisterBuiltinTools hands it to the web
	// tool, and a typed-nil router would pass the nil guard and crash web_search.
	a.initRouterAndModels()
	a.initTools(memDir)
	a.initKernelAndServer(memDir)
}

func (a *AppRunner) initObservabilityAndSecurity() {
	// 1. Boot Core Observability
	observability.InitLogger(!a.Cfg.UIMode)

	// 2. Build the one process sandbox from config and report its status, so it
	// is never a silent no-op. It's wired into the terminal tool in initTools.
	a.Sandbox = security.NewSandboxForMode(
		security.ParseMode(a.Cfg.ResolvedSandboxMode()), a.Cfg.SandboxWritable, a.Emitter)
	observability.Log().Info(a.Sandbox.Status(), nil)
	if a.Sandbox.Mode != security.ModeOff && !a.Sandbox.Available() {
		observability.Log().Warn("sandbox requested but no backend (bwrap/firejail) found — shell commands run unconfined; install bubblewrap or set sandbox to off to silence", nil)
	}

	// 3. Air-gap: refuse every connection that leaves the machine. Enforced at
	// dial time, so it holds for redirects and for provider calls alike.
	if a.Cfg.AirGap {
		safeurl.SetAirGap(true)
		observability.Log().Info("air-gap mode on — outbound network access is blocked (loopback/private still reachable)", nil)
	}

	// 4. Discover and Load External Plugins
	a.PluginHost = plugin.NewHost()
	a.PluginLoader = plugin.NewLoader(a.PluginHost, "./plugins")
	_ = a.PluginLoader.DiscoverAll()
}

// pingModelAsyncTimeout bounds a single connectivity probe — long enough to
// reach a working endpoint, short enough to fail fast on a broken one.
const pingModelAsyncTimeout = 5 * time.Second

// pingModelAsync runs a non-blocking connectivity check and logs a warning on
// failure, so a misconfigured endpoint surfaces at startup rather than on the
// first chat. Never blocks startup and never prevents model registration.
func pingModelAsync(client core.LLMClient, label string) {
	if client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), pingModelAsyncTimeout)
		defer cancel()
		if err := client.Ping(ctx); err != nil {
			observability.Log().Warn("model connectivity check failed — verify the base URL and API key",
				map[string]interface{}{"model": label, "error": err.Error()})
		}
	}()
}

// defaultDarkcodeDir is the single source of truth for DarkCode's system-wide
// state root "~/.darkcode/<name>" (config, memory, projects, binaries, models),
// falling back to CWD-relative only when the home directory can't be resolved.
func defaultDarkcodeDir(name string) string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".darkcode", name)
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".darkcode", name)
}

func (a *AppRunner) initMemoryAndProjects() string {
	a.Registry = tools.NewRegistry()
	a.SourceMgr = tools.NewSourceManager(a.Registry)

	resolveDataDir := func(cfgDir, name string) string {
		fallback := defaultDarkcodeDir(name)
		if cfgDir == "" || cfgDir == fallback {
			return fallback
		}
		if err := os.MkdirAll(cfgDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: configured %s_dir %q unusable (%v); using %s\n", name, cfgDir, err, fallback)
			return fallback
		}
		return cfgDir
	}

	memDir := resolveDataDir(a.Cfg.MemoryDir, "memory")
	var err error
	a.MemSystem, err = memory.NewSystem(memDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing memory: %v\n", err)
		os.Exit(1)
	}

	projDir := resolveDataDir(a.Cfg.ProjectsDir, "projects")
	a.ProjectStore, err = project.NewStore(projDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing project store: %v\n", err)
		os.Exit(1)
	}
	return memDir
}

func (a *AppRunner) initTools(memDir string) {
	oldStore, err := memory.NewStore(filepath.Join(memDir, "memory.json"))
	if err != nil {
		oldStore = nil
	}
	backend, err := tools.NewBackend(a.Cfg.ExecutionBackend, a.Cfg.ExecutionImage,
		a.Cfg.ExecutionHost, a.Cfg.ExecutionPort, a.Sandbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v; falling back to local execution\n", err)
		backend = tools.LocalBackend{Sandbox: a.Sandbox}
	}
	if backend.Name() != "local" {
		observability.Log().Info("shell commands run on "+backend.Name(), nil)
	}
	tools.RegisterBuiltinTools(a.Registry, oldStore, a.Router, a.Sandbox, backend)
	tools.RegisterMemoryTool(a.Registry, tools.NewSemanticMemoryTool(oldStore, a.MemSystem))
	tools.RegisterProjectTools(a.Registry, a.ProjectStore)
	a.Registry.Register(ingest.NewIngestTool(a.MemSystem, a.MemSystem.KG()))

	deterministic.RegisterAll(a.Registry)

	// Register the KG re-sync tool and run an initial background sync so the
	// graph holds typed symbol/import facts from boot. Async so a large
	// workspace never delays startup.
	deterministicKG := a.MemSystem.KG()
	a.Registry.Register(deterministic.NewKGSyncTool(deterministicKG))
	cwd, _ := os.Getwd()
	if kg, ok := deterministicKG.(*memory.KnowledgeGraph); ok {
		// The health daemon watches structure in the background so a cycle or
		// a coupling trend is noticed when it appears, not when somebody
		// happens to run a report. It holds itself to a share of one core, so
		// it stays out of the way of the machine the user is working on.
		a.HealthDaemon = memory.NewHealthDaemon(kg, a.Cfg.MemoryDir)
		a.HealthDaemon.SetCPUPercent(a.Cfg.HealthCPUPercent)
		if a.Cfg.HealthDaemon {
			a.HealthDaemon.Start(context.Background())
		}
		// Conventions mined here are kept in a library that outlives this
		// repository, so a rule followed in one codebase can be checked in
		// the next.
		a.Patterns = memory.NewPatternLibrary(a.Cfg.MemoryDir)
		a.Patterns.Learn(cwd, kg.MinePatterns(cwd))
		tools.RegisterGraphTool(a.Registry, kg, cwd, a.HealthDaemon, a.Patterns)
		// Trying several fixes and keeping the one that passes needs each to be
		// applied and rolled back, which the agent cannot do with write_file.
		tools.RegisterCandidateTool(a.Registry, cwd, kg, a.Patterns, candidate.DefaultVerify(cwd))
		// Structural findings become branches only once the verifier has passed
		// with the change applied. Nothing is pushed.
		tools.RegisterSelfHealTool(a.Registry, cwd, kg, a.Patterns, candidate.DefaultVerify(cwd))
	}
	tools.RegisterGitHubTool(a.Registry, cwd)
	// One call for search-read-summarise, instead of the model spending a turn
	// on each. Shares the SSRF-guarded client every other outbound path uses.
	tools.RegisterResearchTool(a.Registry, safeurl.EgressClient(60*time.Second), a.Router)
	// A language server answers the type-aware questions the graph cannot.
	// Servers start lazily on first query, so this costs nothing when none is
	// installed.
	a.LSP = tools.RegisterLSPTool(a.Registry, cwd)
	// Reading runtime values beats guessing at them; needs delve on PATH and
	// reports so plainly when it is missing.
	tools.RegisterDebugTool(a.Registry, cwd)
	go func() {
		cwd, err := os.Getwd()
		if err != nil {
			return
		}
		if stats, err := deterministic.SyncWorkspaceKG(context.Background(), cwd, deterministicKG); err == nil {
			observability.Log().Debug("knowledge graph code index synced", map[string]interface{}{
				"files": stats.Files, "symbols": stats.Symbols,
				"packages": stats.Packages, "edges": stats.Edges,
			})
		}
	}()

	for _, entry := range a.Registry.List() {
		if entry.Source == "" {
			entry.Source = "builtin"
		}
	}

	for _, sc := range a.Cfg.ToolSources {
		_, _ = a.SourceMgr.Add(tools.SourceConfig{
			ID:          sc.ID,
			Name:        sc.Name,
			Type:        tools.SourceType(sc.Type),
			Command:     sc.Command,
			Args:        sc.Args,
			Env:         sc.Env,
			URL:         sc.URL,
			Headers:     sc.Headers,
			Path:        sc.Path,
			AutoConnect: sc.AutoConnect,
		})
	}
	if len(a.Cfg.ToolSources) > 0 {
		_ = a.SourceMgr.ConnectAll(context.Background())
	}
}

func (a *AppRunner) initRouterAndModels() {
	routingMode := core.ParseRoutingMode(a.Cfg.RoutingMode)
	if a.GuiFlag {
		a.Emitter = ui.NewSSEEventEmitter()
		a.Cfg.UIMode = true
	} else {
		if a.Cfg.UIMode {
			a.Emitter = ui.NewEventEmitter(true, os.Stderr)
		} else {
			a.Emitter = ui.NewEventEmitter(false, os.Stderr)
		}
	}

	if a.Emitter != nil {
		emitterForMetrics := a.Emitter
		metrics.Default.SetOnRecord(func(rec metrics.RequestRecord) {
			snap := metrics.Default.Snapshot()
			emitterForMetrics.EmitTokenUsage(core.TokenUsageStats{
				Model:            rec.Model,
				Provider:         rec.Provider,
				PromptTokens:     rec.PromptTokens,
				CompletionTokens: rec.CompletionTokens,
				TotalTokens:      rec.TotalTokens,
				Cost:             rec.Cost,
				LatencyMs:        rec.LatencyMs,
				Stream:           rec.Stream,
				CumulativeTokens: snap.TotalTokens,
				CumulativeCost:   snap.TotalCost,
				CumulativeReqs:   snap.TotalRequests,
			})
		})
	}

	a.Router = router.NewRouter(routingMode, a.Emitter)
	a.Router.SetEnableLocalOffloading(a.Cfg.EnableLocalOffloading)
	// Force-local (LocalMode "force"): pin routing to the local model family so
	// no request can silently fall back to a cloud provider. Bound at startup
	// here; runtime changes go through Kernel.ApplyLocalPreference.
	a.Router.SetForceLocal(a.Cfg.ForceLocal())

	binDir := defaultDarkcodeDir("bin")
	modelsDir := defaultDarkcodeDir("models")

	if caps, err := capability.Detect(context.Background()); err == nil {
		a.Router.SetAdvisor(capability.NewAdvisor(caps))
	}

	// createClient builds an LLM client, returning the *EmbeddedClient itself
	// for the embedded provider so its model-swap guard survives the retry wrap.
	createClient := func(mc config.ModelConfig) core.LLMClient {
		prov, baseURL, apiKey, modelID := mc.Provider, mc.BaseURL, mc.APIKey, mc.Model
		if prov == "embedded" {
			embProv := embedded.NewProviderWithDirs(nil, modelsDir, binDir)

			// Auto-select first available local model if none specified.
			if modelID == "" {
				if models, err := embProv.ListModels(context.Background()); err == nil && len(models) > 0 {
					modelID = models[0].ID
				}
			}

			if modelID != "" {
				if err := embProv.LoadModel(context.Background(), modelID); err == nil {
					if c, err := embProv.CreateClient(modelID, provider.ClientOptions{}); err == nil {
						if emb, ok := c.(*embedded.EmbeddedClient); ok {
							return emb
						}
					}
				} else {
					observability.Log().Warn("embedded model load failed, falling back to standard client", map[string]interface{}{"model": modelID, "error": err.Error()})
				}
			}
		}
		c := llm.NewClient(baseURL, apiKey, modelID)
		c.SetProvider(prov)
		// Pool the configured credentials (the single api_key counts as one) so
		// calls rotate and a throttled key is parked instead of retried.
		c.Keys = llm.NewKeyPool(append([]string{apiKey}, mc.APIKeys...)...)
		c.Effort = mc.ReasoningEffort
		return c
	}

	for _, mc := range a.Cfg.Models {
		t := core.ParseModelTier(mc.Tier)
		client := createClient(mc)
		a.Router.RegisterModel(t, llm.WrapCloud(client, mc.Provider, mc.Model), mc.Model)
		a.Router.SetModelRole(mc.Model, mc.Role)
		pingModelAsync(client, mc.Model)
	}

	endpointUsable := func(provider, baseURL, model string) bool {
		return provider == "embedded" || (model != "" && baseURL != "")
	}

	var primaryClient core.LLMClient
	if a.Cfg.Model != "" || !a.Cfg.EnableLocalLLM {
		primaryClient = createClient(config.ModelConfig{Provider: a.Cfg.Provider, BaseURL: a.Cfg.BaseURL, APIKey: a.Cfg.APIKey, Model: a.Cfg.Model})
		if endpointUsable(a.Cfg.Provider, a.Cfg.BaseURL, a.Cfg.Model) {
			tier := core.PrimaryTierForMode(routingMode)
			a.Router.RegisterModel(tier, llm.WrapCloud(primaryClient, a.Cfg.Provider, a.Cfg.Model), a.Cfg.Model)
			a.Router.MarkPrimary(a.Cfg.Model)
			pingModelAsync(primaryClient, a.Cfg.Model)
		}
	}
	if primaryClient == nil {
		fallbackModel := a.Cfg.Model
		if fallbackModel == "" {
			for _, mc := range a.Cfg.Models {
				if mc.Model != "" {
					fallbackModel = mc.Model
					break
				}
			}
		}
		primaryClient = createClient(config.ModelConfig{Provider: a.Cfg.Provider, BaseURL: a.Cfg.BaseURL, APIKey: a.Cfg.APIKey, Model: fallbackModel})
		// Only register a usable endpoint. An unconfigured client is kept as a
		// compressor placeholder (hot-swapped when the local model loads) but not
		// registered, so the preflight can report that no model is available.
		if primaryClient != nil && endpointUsable(a.Cfg.Provider, a.Cfg.BaseURL, fallbackModel) {
			tier := core.PrimaryTierForMode(routingMode)
			a.Router.RegisterModel(tier, llm.WrapCloud(primaryClient, a.Cfg.Provider, fallbackModel), fallbackModel)
			a.Router.MarkPrimary(fallbackModel)
			pingModelAsync(primaryClient, fallbackModel)
		}
	}

	fastClient := primaryClient
	fastModel := a.Cfg.Model
	if a.Cfg.CompressorModel != "" {
		if mc, ok := a.Cfg.Models[a.Cfg.CompressorModel]; ok {
			fastClient = createClient(mc)
			fastModel = mc.Model
		}
	} else if a.Router.HasModel(core.ModelTierFast) {
		fc, fm := getFastModel(a.Router, a.Cfg)
		fc.SetProvider(a.Cfg.Provider)
		fastClient, fastModel = fc, fm
	}
	a.Compressor = compression.NewCompressor(llm.WrapCloud(fastClient, a.Cfg.Provider, fastModel), fastModel, a.Router)

	// localEmbedClient is the loaded local model's client, captured for the
	// embedder wiring below (Phase C): llama-server already runs with
	// --embedding, so the same process serves /embeddings for free.
	var localEmbedClient core.LLMClient

	if a.Cfg.ResolvedLocalMode() != "off" {
		localEmbedClient = a.loadLocalLLM(routingMode)
	}

	wireEmbedder := func(client core.LLMClient, label string) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := memory.ValidateEmbedder(ctx, client); err != nil {
				observability.Log().Warn("embedder failed quality validation; semantic vectors disabled (keyword recall unchanged)",
					map[string]interface{}{"embedder": label, "error": err.Error()})
				return
			}
			a.MemSystem.SetEmbedder(client)
			observability.Log().Info("memory embedder validated and enabled", map[string]interface{}{"embedder": label})
		}()
	}
	switch a.Cfg.EmbeddingModel {
	case "off":
		// explicitly disabled
	case "":
		if localEmbedClient != nil {
			wireEmbedder(localEmbedClient, "local embedded model")
		}
	default:
		if mc, ok := a.Cfg.Models[a.Cfg.EmbeddingModel]; ok {
			// Raw client (no retry wrapper) — see localEmbedClient comment.
			wireEmbedder(createClient(mc), mc.Model)
		} else {
			observability.Log().Warn("embedding_model not found in models config; embeddings disabled", map[string]interface{}{"model": a.Cfg.EmbeddingModel})
		}
	}

	// Preflight graceful-degradation check: if no model is registered for any
	// tier, surface ONE clear diagnostic instead of letting every sub-agent
	// fail downstream with an opaque "no model available for tier coding".
	// This is guidance only — it does not change failure handling.
	if a.Router != nil && a.Router.ModelCount() == 0 {
		msg := "No LLM is available: enable a local model (run `/local on` and restart) or add a cloud provider in Settings."
		observability.Log().Error("no models registered after initialization", nil, nil)
		if a.Emitter != nil {
			a.Emitter.EmitError(msg)
		}
	}
}

// loadLocalLLM downloads (if needed) the llama-server binary and a
// resource-appropriate default model, loads it, and registers it with the
// router (as primary when no cloud model is configured). Returns the loaded
// model's raw client for embedder wiring, or nil on failure (logged, never
// fatal). Shared by normal startup and the on-demand "/local on" path.
func (a *AppRunner) loadLocalLLM(routingMode core.RoutingMode) core.LLMClient {
	observability.Log().Info("initialising the local llm", nil)
	binDir := defaultDarkcodeDir("bin")
	modelsDir := defaultDarkcodeDir("models")

	if err := embedded.EnsureLlamaServer(context.Background(), binDir); err != nil {
		observability.Log().Warn("auto-downloader for llama-server failed", map[string]interface{}{"error": err.Error()})
	}

	caps, err := capability.Detect(context.Background())
	if err != nil {
		observability.Log().Warn("could not detect capabilities for local models", map[string]interface{}{"error": err.Error()})
		return nil
	}

	// Never-force gate (LocalMode semantics): "auto" additionally requires a
	// hardware tier that can run local models at all; "on" skips the tier
	// check but — like every mode — still goes through the Local Resource
	// Governor below, because launching an over-budget llama-server IS the
	// low-RAM hang. Refusals are logged and surfaced, never silent.
	mode := a.Cfg.ResolvedLocalMode()
	advisor := capability.NewAdvisor(caps)
	if mode == "auto" && !advisor.CanRunLocalModels() {
		reason := fmt.Sprintf("local disabled: hardware tier %s is below the local-model minimum — using cloud only", advisor.Tier())
		observability.Log().Warn(reason, nil)
		embedded.SetLoadRefusal(reason)
		return nil
	}

	// Plumb detected capabilities into the embedded provider so LoadModel
	// can compute a GPU layer count (-ngl) for supported GPUs.
	embedded.SetCapabilities(caps)
	// Plumb the user's context-window override (0 = auto). When set, the
	// embedded provider always launches with this -c value, winning over
	// the RAM-aware default.
	embedded.SetContextSizeOverride(a.Cfg.EffectiveEmbeddedContextSize())
	// Plumb the user's idle-unload timeout (0 = disabled, the default).
	if a.Cfg.EmbeddedIdleTimeoutMinutes > 0 {
		embedded.SetIdleTimeout(time.Duration(a.Cfg.EmbeddedIdleTimeoutMinutes) * time.Minute)
	}

	// Auto-Download: Ensure at least one model exists. The function selects
	// a model appropriate for this system's resources (different RAM/GPU
	// tiers get different-sized models).
	if err := embedded.EnsureDefaultModels(context.Background(), modelsDir, int64(caps.Memory.TotalBytes), int64(caps.GPU.VRAMBytes)); err != nil {
		observability.Log().Warn("auto-downloader for default models failed", map[string]interface{}{"error": err.Error()})
	}

	embProv := embedded.NewProviderWithDirs(nil, modelsDir, binDir)
	models, err := embProv.ListModels(context.Background())
	if err != nil || len(models) == 0 {
		observability.Log().Warn("could not list local models or no models found", map[string]interface{}{"error": fmt.Sprintf("%v", err)})
		return nil
	}

	// The resource governor plans against FREE memory, counting weights + KV
	// cache + LoRAs + overhead, so it won't approve a load that swap-thrashes.
	candidates := make([]embedded.ModelFile, 0, len(models))
	for i := range models {
		candidates = append(candidates, embedded.ModelFile{Path: models[i].ID, Bytes: models[i].SizeBytes})
	}
	plan := embedded.PlanLocalLoad(caps, candidates, embedded.LoRADirBytes(""), a.Cfg.EffectiveEmbeddedContextSize())
	if !plan.Fits {
		observability.Log().Warn("local model refused by resource governor", map[string]interface{}{"reason": plan.Refusal, "mode": mode})
		embedded.SetLoadRefusal(plan.Refusal)
		return nil
	}
	embedded.SetLoadPlan(&plan)
	observability.Log().Info("local load plan", map[string]interface{}{
		"model":            plan.ModelPath,
		"n_ctx":            plan.NCtx,
		"n_parallel":       plan.NParallel,
		"effective_window": plan.EffectiveWindow(),
		"total_gb":         float64(plan.ModelBytes+plan.KVBytes+plan.LoRABytes) / (1 << 30),
	})

	var selected *core.ModelMetadata
	for i := range models {
		if models[i].ID == plan.ModelPath {
			selected = &models[i]
			break
		}
	}
	if selected == nil {
		observability.Log().Warn("governor-selected model missing from listing", map[string]interface{}{"model": plan.ModelPath})
		return nil
	}
	selectedTier := core.ModelTierMediumLocal
	if plan.ModelBytes < 800<<20 {
		selectedTier = core.ModelTierTinyLocal
	}

	var loaded core.LLMClient
	loadLocal := func(m *core.ModelMetadata, tier core.ModelTier, isPrimary bool) {
		if err := embProv.LoadModel(context.Background(), m.ID); err != nil {
			observability.Log().Error("error loading local model", err, map[string]interface{}{"model": m.ID})
			return
		}
		c, err := embProv.CreateClient(m.ID, provider.ClientOptions{})
		if err != nil {
			observability.Log().Error("error creating client for local model", err, map[string]interface{}{"model": m.ID})
			return
		}
		emb, ok := c.(*embedded.EmbeddedClient)
		if !ok {
			return
		}
		// Wrap emb itself (not emb.Client) so RetryingClient calls through
		// EmbeddedClient's model-swap guard instead of bypassing it via the
		// raw inner client.
		wrapped := llm.WithRetry(emb, llm.DefaultRetryOpts)
		a.Router.RegisterModel(tier, wrapped, m.ID)
		if loaded == nil {
			// Raw client, NOT the retry wrapper: embedding calls sit on the
			// recall hot path and must fail fast to the keyword fallback,
			// not retry with backoff.
			loaded = emb
		}
		if isPrimary {
			a.Router.RegisterModel(core.PrimaryTierForMode(routingMode), wrapped, m.ID)
			a.Router.MarkPrimary(m.ID)
			// Hot-swap the compressor onto the local model so STM compression
			// has a client when no cloud primary/fast tier exists yet.
			if a.Compressor != nil {
				a.Compressor.SetClient(wrapped, m.ID)
			}
		}
		observability.Log().Info("loaded local model", map[string]interface{}{"model": m.ID, "tier": string(tier)})
	}

	loadLocal(selected, selectedTier, a.Cfg.Model == "")
	return loaded
}

// loadLocalLLMOnDemand is the Kernel.SetLocalLoader hook: it runs the same
// load path as startup, so runtime callers (/local on, the GUI toggle) can
// start the embedded model without a restart. On failure it returns the
// governor's refusal reason when there is one, rather than failing silently.
func (a *AppRunner) loadLocalLLMOnDemand(ctx context.Context) error {
	if a.loadLocalLLM(core.ParseRoutingMode(a.Cfg.RoutingMode)) != nil {
		return nil
	}
	if reason := embedded.Default().LoadRefusal(); reason != "" {
		return fmt.Errorf("%s", reason)
	}
	return fmt.Errorf("local model could not be loaded (see logs for the download/load step that failed)")
}

func (a *AppRunner) initKernelAndServer(memDir string) {
	routingMode := core.ParseRoutingMode(a.Cfg.RoutingMode)
	orchCfg := orchestrator.DefaultConfig()
	orchCfg.RoutingMode = routingMode
	orchCfg.UIMode = a.Cfg.UIMode
	orchCfg.MaxConcurrent = a.Cfg.MaxConcurrent
	orchCfg.MaxTurns = a.Cfg.MaxTurns
	orchCfg.SafetyLevel = parseSafetyLevel(a.Cfg.SafetyLevel)
	orchCfg.CompressContext = a.Cfg.CompressContext
	orchCfg.UseCtxEngine = a.Cfg.UseCtxEngine
	orchCfg.AgenticLoop = a.Cfg.AgenticLoop
	orchCfg.MaxLoops = a.Cfg.MaxLoops
	orchCfg.ExecutionProfile = a.Cfg.ExecutionProfile
	orchCfg.PlanApproval = a.Cfg.PlanApproval
	orchCfg.PlanDepth = a.Cfg.PlanDepth
	orchCfg.ContextLength = a.Cfg.ContextLength
	orchCfg.UseLocalForAux = a.Cfg.UseLocalForAux
	orchCfg.PostLoopConsensus = a.Cfg.PostLoopConsensus

	a.Kernel = orchestrator.New(orchCfg, a.Router, a.Registry, a.MemSystem, a.Compressor, a.Emitter)
	a.Recorder = tools.NewChangeRecorder()
	a.Kernel.SetChangeRecorder(a.Recorder)

	// Checkpoints: snapshot the workspace before every mutating tool so the
	// user can undo the agent. The blob store is shared across projects (files
	// are content-addressed), so this is cheap to keep always-on. A failure
	// here costs the undo history, not the session.
	if cwd, err := os.Getwd(); err == nil {
		if m, err := checkpoint.New(defaultDarkcodeDir("checkpoints"), cwd); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: checkpoints unavailable (%v); /rollback is disabled\n", err)
		} else {
			m.SetTurnFunc(func() int { return len(a.MemSystem.STMGet()) })
			a.Checkpoints = m
			a.Kernel.SetCheckpoints(m)
		}
	}

	// Inject the on-demand local-model loader so /local force / on and the GUI
	// toggle can start the embedded model at runtime (embedded loading lives
	// in package main; the kernel can't import it directly). See
	// Kernel.ApplyLocalPreference.
	a.Kernel.SetLocalLoader(a.loadLocalLLMOnDemand)

	// Persist the cascade rung log next to the rest of memory so the
	// threshold-calibration dataset (which rung answered what, and which
	// local answers the user rejected by re-asking) survives restarts.
	a.Kernel.SetCascadeLogPath(filepath.Join(memDir, "cascade_log.jsonl"))

	// Journal DAG runs so a crashed multi-step task resumes from where it
	// stopped instead of re-paying for every completed sub-task.
	a.Kernel.SetRunsDir(defaultDarkcodeDir("runs"))

	// Per-model reliability accumulates across sessions, so a model that keeps
	// failing a role stops being asked to do it.
	a.Router.SetReliabilityPath(filepath.Join(memDir, "model_reliability.json"))

	// Cost governor: enforce optional spend caps against the process-wide
	// usage tracker. Only installed when a budget is actually configured, so
	// the default (no caps) means zero enforcement overhead.
	budget := metrics.BudgetLimits{
		PerSessionUSD: a.Cfg.CostLimitPerSessionUSD,
		PerDayUSD:     a.Cfg.CostLimitPerDayUSD,
		Action:        metrics.ParseBudgetAction(a.Cfg.CostLimitAction),
	}
	if budget.Configured() {
		a.Kernel.SetCostGovernor(metrics.NewCostGovernor(metrics.Default, budget))
	}

	gate := a.Kernel.Gate()
	gate.SetDenyRules(a.Cfg.DenyRules)
	gate.SetAskTimeout(time.Duration(a.Cfg.ApprovalTimeoutSeconds) * time.Second)
	// Smart approvals: ask the auxiliary model whether a flagged low-risk call
	// is routine. Anything other than a clear "safe" — including an error, a
	// timeout, or an unparseable reply — falls through to the user.
	if a.Cfg.SmartApproval {
		gate.SetJudge(func(req permission.ApprovalRequest) bool {
			client, model, err := a.Kernel.PlannerClient()
			if err != nil || client == nil {
				return false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
				Model:    model,
				Messages: []core.Message{{Role: core.RoleUser, Content: permission.JudgePrompt(req)}},
			})
			if err != nil || resp == nil || len(resp.Choices) == 0 {
				return false
			}
			return permission.ParseJudgeVerdict(resp.Choices[0].Message.Content)
		})
	}

	// Let the code graph escalate edits to structurally central files.
	if kg, ok := a.MemSystem.KG().(*memory.KnowledgeGraph); ok && a.Cfg.BlastRadiusThreshold > 0 {
		gate.SetBlastRadius(func(path string) float64 {
			return kg.BlastRadius([]string{path}, 2).Severity
		}, a.Cfg.BlastRadiusThreshold)
	}
	gate.OnDecision(func(req permission.ApprovalRequest, d permission.Decision) {
		if a.MemSystem != nil && a.MemSystem.Audit() != nil {
			approved := d != permission.DecisionDeny
			outcome := d.String()
			if !approved {
				outcome = "denied"
			}
			_ = a.MemSystem.Audit().RecordAction(
				core.RoleExecutive, "permission:"+req.Tool, req.Tool,
				req.Risk, approved, outcome,
			)
		}
		if a.Emitter != nil {
			a.Emitter.Emit(core.EventApproval, map[string]interface{}{
				"tool":     req.Tool,
				"summary":  req.Summary,
				"risk":     string(req.Risk),
				"decision": d.String(),
			}, ui.WithTool(req.Tool), ui.WithStatus("decided"))
		}
	})

	serverApprover := permission.NewServerApprover()
	if a.Emitter != nil {
		serverApprover.OnRequest(func(id string, req permission.ApprovalRequest) {
			a.Emitter.Emit(core.EventApproval, map[string]interface{}{
				"id":      id,
				"tool":    req.Tool,
				"summary": req.Summary,
				"preview": req.Preview,
				"risk":    string(req.Risk),
			}, ui.WithTool(req.Tool), ui.WithStatus("request"))
		})
	}
	modeApprover := permission.NewModeAwareApprover(serverApprover)
	gate.SetApprover(modeApprover.Approve)
	a.Kernel.SetModeApprover(modeApprover)
	a.Server = server.NewServer(a.Cfg, a.Registry, a.MemSystem, a.Emitter, a.Kernel, serverApprover, a.ProjectStore, a.SourceMgr)
}
