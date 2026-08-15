package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkcode/candidate"
	"github.com/darkcode/capability"
	"github.com/darkcode/checkpoint"
	"github.com/darkcode/compression"
	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/hooks"
	"github.com/darkcode/ingest"
	"github.com/darkcode/llm"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/modelport"
	"github.com/darkcode/observability"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/plugin"
	"github.com/darkcode/project"
	"github.com/darkcode/provider"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/recall"
	"github.com/darkcode/router"
	"github.com/darkcode/safeurl"
	"github.com/darkcode/security"
	"github.com/darkcode/server"
	"github.com/darkcode/spill"
	"github.com/darkcode/tools"
	"github.com/darkcode/tools/deterministic"
	"github.com/darkcode/ui"
	"github.com/darkcode/uiport"
)

func (a *AppRunner) WireUp() {
	a.loadPolicy()
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
	observability.InitLogger(!a.Cfg.UIMode, filepath.Join(defaultDarkcodeDir("logs"), "darkcode.log"))

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
	a.loadExtensions()
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
	// The memory gateway comes first: every tool that remembers anything is
	// handed it, so placement is one decision rather than one per caller.
	rec, recErr := recall.New(a.MemSystem)
	if recErr != nil {
		fmt.Fprintf(os.Stderr, "Fatal: %v\n", recErr)
		os.Exit(1)
	}
	a.Recall = rec

	oldStore, err := memory.NewStore(filepath.Join(memDir, "memory.json"))
	if err != nil {
		oldStore = nil
	}
	backend, err := tools.NewBackend(a.Cfg.ExecutionBackend, a.Cfg.ExecutionImage,
		a.Cfg.ExecutionHost, a.Cfg.ExecutionPort, a.Sandbox)
	if err != nil {
		// Not a fallback to local. Someone who asked for docker or ssh and got
		// local without noticing would believe their commands were isolated
		// when they were not, and the warning goes to a stderr the GUI user
		// never sees. The terminal tool refuses instead, so the error arrives
		// where they are actually working.
		fmt.Fprintf(os.Stderr, "Warning: %v; shell commands will be refused until this is fixed\n", err)
		observability.Log().Error("execution backend unusable", err, nil)
		backend = tools.MisconfiguredBackend{Err: err}
	}
	if backend.Name() != "local" {
		observability.Log().Info("shell commands run on "+backend.Name(), nil)
	}
	tools.RegisterBuiltinTools(a.Registry, oldStore, a.Router, a.Sandbox, backend)
	memTool := tools.NewSemanticMemoryTool(oldStore, a.MemSystem)
	memTool.Recall = rec
	tools.RegisterMemoryTool(a.Registry, memTool)
	tools.RegisterProjectTools(a.Registry, a.ProjectStore)
	a.Registry.Register(ingest.NewIngestTool(a.MemSystem, a.MemSystem.KG(), rec))

	deterministic.RegisterAll(a.Registry)

	// Oversized tool results are offloaded to disk rather than truncated away.
	// A 200 KB file read used to reach the model as 4 KB with the remainder
	// destroyed; now it arrives as a head/tail preview with a handle, and
	// read_result pages through the rest. Failing to open the store is not
	// fatal — without it the registry falls back to truncating, which is what
	// happened before.
	if st, err := spill.New(filepath.Join(memDir, "spill")); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: large tool results will be truncated rather than offloaded (%v)\n", err)
	} else {
		a.Registry.SetSpillStore(st)
		tools.RegisterSpillTool(a.Registry, st)
	}

	// What the agent reads and writes is what it knows the state of. Recording
	// the content hash per file is what lets the graph answer "which of my
	// beliefs are about a version of this file that no longer exists" exactly,
	// including for edits the agent made itself and has not committed.
	if ws, err := os.Getwd(); err == nil {
		mem := a.MemSystem
		a.Registry.SetFileObserver(func(path, content string) {
			mem.ObserveFile(ws, path, content)
		})
	}

	// Register the KG re-sync tool and run an initial background sync so the
	// graph holds typed symbol/import facts from boot. Async so a large
	// workspace never delays startup.
	deterministicKG := a.MemSystem.KG()
	if w := recall.Graph(rec); w != nil {
		deterministicKG = w
	}
	a.Registry.Register(deterministic.NewKGSyncTool(deterministicKG))
	cwd, _ := os.Getwd()
	if kg, ok := a.MemSystem.KG().(*memory.KnowledgeGraph); ok {
		// The health daemon watches structure in the background so a cycle or
		// a coupling trend is noticed when it appears, not when somebody
		// happens to run a report. It holds itself to a share of one core, so
		// it stays out of the way of the machine the user is working on.
		a.HealthDaemon = memory.NewHealthDaemon(kg, a.Cfg.MemoryDir)
		a.HealthDaemon.SetCPUPercent(a.Cfg.HealthCPUPercent)
		if a.Cfg.HealthDaemonEnabled() {
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
	// Guarded because this parses whatever source the workspace happens to
	// contain. An index that cannot read one file should lose that file, not the
	// session — on a bare goroutine a parse panic ends the whole process.
	observability.Go("kg-code-index", func() {
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
	})

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
	// Persist it so the kernel's live model-reload (ReloadModels) can build
	// clients through the same factory instead of importing llm itself.
	a.createClient = createClient

	for _, mc := range a.Cfg.Models {
		t := core.ParseModelTier(mc.Tier)
		client := createClient(mc)
		a.registerModel(t, llm.WrapCloud(client, mc.Provider, mc.Model), mc.Provider, mc.Model)
		a.Router.SetModelRole(mc.Model, mc.Role)
		pingModelAsync(client, mc.Model)
	}

	endpointUsable := func(provider, baseURL, model string) bool {
		return provider == "embedded" || (model != "" && baseURL != "")
	}

	var primaryClient core.LLMClient
	if a.Cfg.Model != "" || !a.Cfg.LocalEnabled() {
		primaryClient = createClient(config.ModelConfig{Provider: a.Cfg.Provider, BaseURL: a.Cfg.BaseURL, APIKey: a.Cfg.APIKey, Model: a.Cfg.Model})
		if endpointUsable(a.Cfg.Provider, a.Cfg.BaseURL, a.Cfg.Model) {
			tier := core.PrimaryTierForMode(routingMode)
			a.registerModel(tier, llm.WrapCloud(primaryClient, a.Cfg.Provider, a.Cfg.Model), a.Cfg.Provider, a.Cfg.Model)
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
			a.registerModel(tier, llm.WrapCloud(primaryClient, a.Cfg.Provider, fallbackModel), a.Cfg.Provider, fallbackModel)
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
		a.registerModel(tier, wrapped, localProviderID, m.ID)
		if loaded == nil {
			// Raw client, NOT the retry wrapper: embedding calls sit on the
			// recall hot path and must fail fast to the keyword fallback,
			// not retry with backoff.
			loaded = emb
		}
		if isPrimary {
			a.registerModel(core.PrimaryTierForMode(routingMode), wrapped, localProviderID, m.ID)
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
	orchCfg.ExecutionProfile = a.Cfg.ExecutionProfile
	orchCfg.PlanApproval = a.Cfg.PlanApproval
	orchCfg.PlanDepth = a.Cfg.PlanDepth
	orchCfg.ContextLength = a.Cfg.ContextLength
	orchCfg.UseLocalForAux = a.Cfg.UseLocalForAux

	a.Kernel = orchestrator.New(orchCfg, a.Router, a.Registry, a.MemSystem, a.Compressor, a.Emitter)
	// Hand the kernel the client factory so ReloadModels can rebuild clients on
	// a live config change without the orchestrator importing llm.
	a.Kernel.SetClientFactory(a.createClient)
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

	// Debate is a property of consensus fan-out; the kernel gate for it is
	// runtime state, so the configured value has to be pushed in at startup or
	// the setting would be one nothing reads.
	a.Kernel.SetDebate(a.Cfg.Debate)

	// Same reason as debate, and more urgently: without this line the reviewer
	// cannot run at all. SetReviewer had no caller outside tests, so 173 lines
	// wired into the execute path were unreachable in a shipped binary.
	a.Kernel.SetReviewer(a.Cfg.Reviewer)

	// Placement is one decision. Without this the kernel writes the stores
	// directly, which is correct but is what made it thirty-two decisions.
	a.Kernel.SetRecall(a.Recall)

	// Consolidation at the session boundary: the store is trimmed when a chat
	// ends, which is the moment there is nothing in flight to disturb and the
	// same boundary that already clears short-term memory. Registered before
	// the hooks block so it runs whether or not any hook is configured.
	a.MemSystem.OnNewSession(func() {
		if n := a.MemSystem.Consolidate(a.Cfg.EpisodicMaxEntries); n > 0 {
			fmt.Fprintf(os.Stderr, "Consolidated memory: forgot %d unused entries\n", n)
		}
	})

	// Written-down procedure, loaded at startup rather than waiting for
	// someone to type `/skills import`. The importer has existed all along;
	// nothing called it, so a fresh install stayed ignorant of every runbook
	// on the machine until a user knew the command existed.
	a.importSkills()

	// Now that the registry exists, the bundles loaded at startup can be
	// connected: their tools registered, their commands offered by the console,
	// their hooks folded in with the configured ones.
	ext := a.connectExtensions()
	a.ExtCommands = ext.Commands

	// Lifecycle hooks: built once here and handed to each owner of a point.
	// The registry owns the tool points because it owns tool execution; the
	// kernel owns the compaction boundary; uiport carries turn_end for every
	// surface (see app_postturn.go); memory announces the session boundary.
	//
	// A bad hooks block is a startup error rather than a warning: a hook filed
	// under a misspelled point would never fire and never complain, which is
	// exactly the silent-no-op failure this codebase has been bitten by before.
	if h, err := hooks.New(mergeHooks(hookConfig(a.Cfg.Hooks), ext.Hooks)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	} else if h != nil {
		a.Hooks = h
		h.SetLog(func(m string) { fmt.Fprintf(os.Stderr, "hook: %s\n", m) })
		a.Registry.SetHooks(h)
		a.Kernel.SetHooks(h)
		a.MemSystem.OnNewSession(func() {
			_ = h.Run(context.Background(), hooks.SessionStart, hooks.Context{})
		})
	}

	// The auxiliary ladder in modelport reads this: with it on, summarising and
	// classifying prefer a healthy local model before any metered one. It is
	// the setting RouteAux used to consult.
	a.Kernel.PreferLocalForAux(a.Cfg.UseLocalForAux)

	// The one door from any surface into the kernel. Built here so every
	// surface shares it and none can construct a request the others wouldn't.
	port, err := uiport.New(a.Kernel, a.newPostTurnHooks()...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal: %v\n", err)
		os.Exit(1)
	}
	a.Port = port

	gate := a.Kernel.Gate()
	gate.SetDenyRules(a.Cfg.DenyRules)
	gate.SetAllowedTools(a.Policy.Tools.AllowOnly)
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
			// An approval verdict is a closed question. It ran unbounded and
			// at the model's default temperature, so the call that decides
			// whether a dangerous tool runs could return an essay and could
			// return a different answer to the same question twice. Classify
			// is deterministic and short by policy.
			_, maxTok, temp := modelport.PolicyFor(modelport.PurposeClassify)
			resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
				Model:       model,
				Messages:    []core.Message{{Role: core.RoleUser, Content: permission.JudgePrompt(req)}},
				MaxTokens:   &maxTok,
				Temperature: &temp,
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
	a.Server = server.NewServer(a.Cfg, a.Registry, a.MemSystem, a.Emitter, a.Kernel, a.Port, serverApprover, a.ProjectStore, a.SourceMgr)
}

// localProviderID names the built-in provider that serves models running on
// this machine, so a policy asking for local-only can recognise them.
const localProviderID = "embedded"

// registerModel adds a model to the router unless policy forbids it.
//
// The check sits at registration rather than at routing: a model that never
// enters the router cannot be reached by any path — not the tier lookup, not
// consensus fan-out, not a role selector — so there is one place to get right
// instead of one per call site.
//
// A refusal is logged. "No model available for tier reasoning" is a baffling
// thing to read when the cause is a policy two directories away.
func (a *AppRunner) registerModel(tier core.ModelTier, client core.LLMClient, provider, model string) {
	if ok, why := a.Policy.ModelAllowed(provider, model); !ok {
		fmt.Fprintf(os.Stderr, "policy: not registering %s/%s — %s\n", provider, model, why)
		return
	}
	a.Router.RegisterModel(tier, client, model)
}

// policyFileName is the policy read from the workspace, then from the install
// root. A repository can tighten what the install permits; it can never widen
// it, because Apply only ever moves a setting in the restrictive direction.
const policyFileName = "policy.json"

// loadPolicy reads the restriction set and folds it into the configuration.
//
// It runs before anything else in WireUp, since every later step reads the
// config it tightens. A malformed policy is fatal rather than ignored: the
// difference between "no policy" and "a policy nobody could parse" is the
// difference between an install that was never restricted and one that thinks
// it is.
func (a *AppRunner) loadPolicy() {
	paths := []string{filepath.Join(".darkcode", policyFileName)}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".darkcode", policyFileName))
	}

	for _, path := range paths {
		p, err := config.LoadPolicy(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if p.Empty() {
			continue
		}
		a.Policy = p
		p.Apply(a.Cfg)
		fmt.Fprintf(os.Stderr, "policy: %s applied\n", path)
		return
	}
}

// hookConfig converts the persisted hook blocks into the shapes package hooks
// validates. The two structs are identical on purpose: config does not import a
// package that shells out, and hooks does not import config.
func hookConfig(cfg map[string][]config.HookConfig) map[string][]hooks.Hook {
	if len(cfg) == 0 {
		return nil
	}
	out := make(map[string][]hooks.Hook, len(cfg))
	for point, list := range cfg {
		for _, h := range list {
			out[point] = append(out[point], hooks.Hook{Match: h.Match, Run: h.Run, Timeout: h.Timeout})
		}
	}
	return out
}

// defaultSkillDirs are searched when the config names none: one per-user, one
// per-workspace. Neither has to exist — a missing directory is the normal case
// and is silently skipped, because "you have no runbooks" is not a warning.
func defaultSkillDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".darkcode", "skills"))
	}
	return append(dirs, defaultDarkcodeDir("skills"))
}

// importSkills loads every configured skill directory into procedural memory.
//
// Failures are reported and never fatal. A malformed runbook should cost that
// runbook, not the session — the same rule the directory walk already applies
// to one unparseable file among twenty.
func (a *AppRunner) importSkills() {
	if a.MemSystem == nil {
		return
	}
	dirs := a.Cfg.SkillDirs
	if len(dirs) == 0 {
		dirs = defaultSkillDirs()
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		dir = expandHome(dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		found, err := a.MemSystem.ImportSkills(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skills in %s: %v\n", dir, err)
			continue
		}
		if n := memory.CountImported(found); n > 0 {
			fmt.Fprintf(os.Stderr, "Loaded %d skill(s) from %s\n", n, dir)
		}
	}
}

// expandHome resolves a leading ~ so a config may write ~/.darkcode/skills.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// defaultExtensionDirs are searched for bundles: one per-user, one
// per-workspace, plus ./plugins for anything installed before extensions had a
// home of their own.
func defaultExtensionDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".darkcode", "extensions"))
	}
	return append(dirs, defaultDarkcodeDir("extensions"), "./plugins")
}

// loadExtensions discovers bundles and connects what they declare.
//
// The connecting is the point. The host has always loaded plugins and stored
// them; nothing read the manifests back, so a bundle declaring three tools
// loaded cleanly, listed in /plugins, and could not be called. Registering the
// tools is what makes an extension an extension.
//
// A load failure is now reported. It used to be assigned to _, so a bundle that
// failed its handshake was indistinguishable from one that was never there.
func (a *AppRunner) loadExtensions() {
	dirs := a.Cfg.ExtensionDirs
	if len(dirs) == 0 {
		dirs = defaultExtensionDirs()
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		dir = expandHome(dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		loader := plugin.NewLoader(a.PluginHost, dir)
		if err := loader.DiscoverAll(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: extensions in %s: %v\n", dir, err)
		}
		a.PluginLoader = loader
	}
}

// connectExtensions registers the loaded bundles' tools and returns their
// commands and hooks. Split from loadExtensions because the registry and the
// hook manager do not exist yet when the bundles are loaded.
func (a *AppRunner) connectExtensions() tools.Extensions {
	ext := tools.RegisterExtensions(a.Registry, a.PluginHost)
	for _, r := range ext.Rejected {
		fmt.Fprintf(os.Stderr, "Warning: extension: %s\n", r)
	}
	if n := len(ext.Tools); n > 0 {
		fmt.Fprintf(os.Stderr, "Extensions: registered %d tool(s)\n", n)
	}
	return ext
}

// mergeHooks folds a bundle's hooks in with the user's own.
//
// The user's come first at each point, so a configured gate runs before an
// extension's and can refuse without the extension ever executing. An
// extension that could pre-empt the config would be an extension that can
// disable the user's own guard.
func mergeHooks(cfg map[string][]hooks.Hook, ext []tools.ExtensionHook) map[string][]hooks.Hook {
	if len(ext) == 0 {
		return cfg
	}
	if cfg == nil {
		cfg = map[string][]hooks.Hook{}
	}
	for _, e := range ext {
		cfg[e.Point] = append(cfg[e.Point], hooks.Hook{Match: e.Match, Run: e.Run, Timeout: e.Timeout})
	}
	return cfg
}
