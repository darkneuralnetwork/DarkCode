package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/darkcode/capability"
	"github.com/darkcode/compression"
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
	"github.com/darkcode/security"
	"github.com/darkcode/server"
	"github.com/darkcode/tools"
	"github.com/darkcode/tools/deterministic"
	"github.com/darkcode/ui"
)

func (a *AppRunner) WireUp() {
	a.initObservabilityAndSecurity()
	memDir := a.initMemoryAndProjects()
	// Router BEFORE tools: RegisterBuiltinTools hands the router to the web
	// tool, so a.Router must already exist — otherwise a nil *router.Router is
	// boxed into the core.ModelRouter interface (a non-nil typed-nil), the web
	// tool's `if t.Router != nil` guard passes, and the first web_search call
	// dereferences nil and crashes the whole server. initRouterAndModels does
	// not depend on the registry, so this ordering is safe.
	a.initRouterAndModels()
	a.initTools(memDir)
	a.initKernelAndServer(memDir)
}

func (a *AppRunner) initObservabilityAndSecurity() {
	// 1. Boot Core Observability
	observability.InitLogger(!a.Cfg.UIMode)

	// 2. Initialize Enterprise Sandbox
	a.Sandbox = security.NewSandbox(a.Emitter)

	// 3. Discover and Load External Plugins
	a.PluginHost = plugin.NewHost()
	a.PluginLoader = plugin.NewLoader(a.PluginHost, "./plugins")
	_ = a.PluginLoader.DiscoverAll()
}

// defaultDarkcodeDir returns the system-wide "~/.darkcode/<name>" path,
// falling back to a CWD-relative one only if the home directory can't be
// resolved (e.g. a minimal container with no HOME set). This is the single
// source of truth for where DarkCode's downloaded/generated state lives —
// config, memory, projects, the llama-server binary, and GGUF/LoRA model
// files all resolve through this so nothing is scattered across whichever
// directory the binary happens to be launched from (previously bin/models
// were always CWD-relative while memory/projects were home-relative — three
// different roots for what should be one system directory).
// pingModelAsyncTimeout bounds a single connectivity probe — short, since
// this only needs to detect an obviously broken endpoint (wrong URL, dead
// key, unreachable host), not tolerate a slow-but-working one.
const pingModelAsyncTimeout = 5 * time.Second

// pingModelAsync fires a non-blocking connectivity check against client and
// logs a clear warning if it fails — local-first upgrade §4c: previously
// core.LLMClient.Ping was declared and implemented but never called
// anywhere, so a misconfigured OpenAI-compatible endpoint (wrong base URL,
// dead API key) was only discovered when a real chat request failed later.
// Async and non-blocking by design: startup must never wait on a
// potentially slow or hanging network call, and a model that's temporarily
// unreachable at boot may still recover — this only surfaces the problem
// early, it never prevents registration or blocks the app.
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
	tools.RegisterBuiltinTools(a.Registry, oldStore, a.Router)
	tools.RegisterMemoryTool(a.Registry, tools.NewSemanticMemoryTool(oldStore, a.MemSystem))
	tools.RegisterProjectTools(a.Registry, a.ProjectStore)
	// Knowledge ingestion (Phase 3): let the agent learn from files/repos/URLs.
	a.Registry.Register(ingest.NewIngestTool(a.MemSystem, a.MemSystem.KG()))

	// Deterministic toolchain (spec §8) — real ripgrep + go/ast backed tools.
	// Registered separately to avoid an import cycle (the deterministic
	// package imports tools.ToolEntry).
	deterministic.RegisterAll(a.Registry)

	// Knowledge-graph code index (local-first upgrade Phase B): register the
	// on-demand re-sync tool and run an initial background sync so the KG
	// holds typed symbol/import facts with provenance from boot. Async so a
	// large workspace never delays startup; the cascade's graph rung simply
	// answers from whatever has been indexed so far.
	deterministicKG := a.MemSystem.KG()
	a.Registry.Register(deterministic.NewKGSyncTool(deterministicKG))
	go func() {
		cwd, err := os.Getwd()
		if err != nil {
			return
		}
		if stats, err := deterministic.SyncWorkspaceKG(context.Background(), cwd, deterministicKG); err == nil {
			observability.Log().Info("knowledge graph code index synced", map[string]interface{}{
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

	// binDir/modelsDir are where the auto-downloaded llama-server binary and
	// GGUF/LoRA model files live — system-wide under ~/.darkcode (see
	// defaultDarkcodeDir) so one download is shared across every directory
	// the binary is launched from, instead of re-downloading per-CWD.
	// binDir is computed once here so every embedded-provider call site
	// (createClient below and the auto-load block) configures the shared
	// singleton with the same dir; previously createClient passed "" and
	// could not find the downloaded binary, while the auto-load block passed
	// the real dir — two divergent wirings of the same provider. Now both go
	// through the singleton.
	binDir := defaultDarkcodeDir("bin")
	modelsDir := defaultDarkcodeDir("models")

	// Wire the capability advisor (spec §1): detect hardware, compute tier,
	// and let the router prefer local models when the system is powerful enough.
	if caps, err := capability.Detect(context.Background()); err == nil {
		a.Router.SetAdvisor(capability.NewAdvisor(caps))
	}

	// Helper to create a client, handling the embedded llama.cpp provider specially.
	// Returns core.LLMClient (not the concrete *llm.Client) so the embedded
	// branch can return the *EmbeddedClient itself — preserving its
	// model-swap guard through llm.WithRetry below, instead of unwrapping to
	// the raw inner client and silently bypassing that guard.
	createClient := func(prov string, baseURL string, apiKey string, modelID string) core.LLMClient {
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
		return c
	}

	for _, mc := range a.Cfg.Models {
		t := core.ParseModelTier(mc.Tier)
		client := createClient(mc.Provider, mc.BaseURL, mc.APIKey, mc.Model)
		a.Router.RegisterModel(t, llm.WithRetry(client, llm.DefaultRetryOpts), mc.Model)
		a.Router.SetModelRole(mc.Model, mc.Role)
		pingModelAsync(client, mc.Model)
	}

	// endpointUsable reports whether a (provider, baseURL, model) triple can
	// actually service requests. An embedded provider is self-contained; every
	// other provider needs BOTH a model name and a base URL. Registering a
	// client without them was the root cause of two failures: requests routed
	// to it died with `unsupported protocol scheme ""` (empty BaseURL), and —
	// worse — the dead registration made Router.ModelCount() > 0, so the
	// "no LLM available" preflight below never fired and the user got an
	// opaque per-request error instead of clear setup guidance.
	endpointUsable := func(provider, baseURL, model string) bool {
		return provider == "embedded" || (model != "" && baseURL != "")
	}

	var primaryClient core.LLMClient
	if a.Cfg.Model != "" || !a.Cfg.EnableLocalLLM {
		primaryClient = createClient(a.Cfg.Provider, a.Cfg.BaseURL, a.Cfg.APIKey, a.Cfg.Model)
		if endpointUsable(a.Cfg.Provider, a.Cfg.BaseURL, a.Cfg.Model) {
			tier := core.PrimaryTierForMode(routingMode)
			a.Router.RegisterModel(tier, llm.WithRetry(primaryClient, llm.DefaultRetryOpts), a.Cfg.Model)
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
		primaryClient = createClient(a.Cfg.Provider, a.Cfg.BaseURL, a.Cfg.APIKey, fallbackModel)
		// Register only a usable endpoint. When nothing is configured yet
		// (empty model/URL and local LLM enabled but not yet loaded), the
		// client is kept as a placeholder for the compressor (hot-swapped once
		// the local model loads) but is NOT registered — so the preflight can
		// correctly report that no model is available.
		if primaryClient != nil && endpointUsable(a.Cfg.Provider, a.Cfg.BaseURL, fallbackModel) {
			tier := core.PrimaryTierForMode(routingMode)
			a.Router.RegisterModel(tier, llm.WithRetry(primaryClient, llm.DefaultRetryOpts), fallbackModel)
			a.Router.MarkPrimary(fallbackModel)
			pingModelAsync(primaryClient, fallbackModel)
		}
	}

	fastClient := primaryClient
	fastModel := a.Cfg.Model
	if a.Cfg.CompressorModel != "" {
		if mc, ok := a.Cfg.Models[a.Cfg.CompressorModel]; ok {
			fastClient = createClient(mc.Provider, mc.BaseURL, mc.APIKey, mc.Model)
			fastModel = mc.Model
		}
	} else if a.Router.HasModel(core.ModelTierFast) {
		fc, fm := getFastModel(a.Router, a.Cfg)
		fc.SetProvider(a.Cfg.Provider)
		fastClient, fastModel = fc, fm
	}
	a.Compressor = compression.NewCompressor(llm.WithRetry(fastClient, llm.DefaultRetryOpts), fastModel, a.Router)

	// localEmbedClient is the loaded local model's client, captured for the
	// embedder wiring below (Phase C): llama-server already runs with
	// --embedding, so the same process serves /embeddings for free.
	var localEmbedClient core.LLMClient

	// --- Auto-load Local LLM (Resource Methodology) ---
	// Factored into loadLocalLLM (below) so the exact same load path is
	// reusable on-demand — see RunCLI's post-WireUp "enable local LLM?"
	// prompt (local-first upgrade: issue 6a) when a user skips setup and
	// ends up with zero configured models. loadLocalLLM handles the
	// compressor hot-swap itself when it registers a primary model, so
	// fastClient/fastModel (only otherwise used to construct a.Compressor
	// above) need no further updates here.
	if a.Cfg.ResolvedLocalMode() != "off" {
		localEmbedClient = a.loadLocalLLM(routingMode)
	}

	// --- Embedder wiring (local-first upgrade Phase C) ---
	// Makes the memory system's dormant vector path live: episodic/semantic
	// writes gain vectors and HybridRetriever.Recall becomes genuinely
	// semantic. Resolution: explicit "off" disables; a named model from
	// cfg.Models wins; otherwise the local embedded model (if one loaded)
	// serves embeddings via llama-server's --embedding endpoint. When nothing
	// resolves, recall keeps its keyword-overlap behavior unchanged.
	//
	// Quality gate (§9 risk): a candidate embedder is wired ONLY after it
	// passes memory.ValidateEmbedder's probe suite — pooled chat-model
	// embeddings can be degenerate, and a bad embedder degrades recall
	// silently. Validation runs async so a warming llama-server never blocks
	// startup; until (and unless) it passes, recall stays on keyword overlap.
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
			wireEmbedder(createClient(mc.Provider, mc.BaseURL, mc.APIKey, mc.Model), mc.Model)
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
// router — as primary when no cloud model is already configured. Returns
// the loaded model's raw client (for embedder wiring) or nil if nothing
// loaded (detection/download/load failure — logged, never fatal).
//
// This is the single load path shared by normal startup
// (initRouterAndModels, when Cfg.EnableLocalLLM is already true) and the
// on-demand "enable local LLM?" prompt (RunCLI, issue 6a) fired when a user
// skipped setup and ends up with zero configured models — so enabling it
// later takes effect immediately without a restart.
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

	// Local Resource Governor: one plan that owns every byte llama-server
	// will consume (weights + KV cache at the planned context + pre-loaded
	// LoRAs + runtime overhead) checked against FREE memory. Replaces the
	// old model-size-only 60% budget, which never saw the KV cache and could
	// approve a load that swap-thrashed the machine.
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
	// Tier by size: the 0.5B-class file registers as tiny_local, anything
	// larger as medium_local (mirrors the previous largest-first assignment).
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
			// Hot-swap the compressor to the now-loaded local model so STM
			// compression runs on it instead of crashing/heuristic-falling
			// back when no cloud primary/fast tier exists yet. Mirrors
			// Kernel.ReloadModels' local-only branch. Safe to call whether
			// this ran at normal startup or on-demand post-WireUp.
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
// load path as startup and reports a clear error when nothing came up, so
// runtime callers (/local force / on, the GUI toggle) can start the embedded
// model without a restart. The ctx is accepted for interface uniformity;
// loadLocalLLM manages its own per-step contexts (download/detect/load), so it
// is not threaded further here. The returned error names the governor's
// refusal reason when there is one — the diagnostic the never-force design
// requires instead of a silent failure.
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
	orchCfg.ContextLength = a.Cfg.ContextLength
	orchCfg.UseLocalForAux = a.Cfg.UseLocalForAux
	orchCfg.PostLoopConsensus = a.Cfg.PostLoopConsensus

	a.Kernel = orchestrator.New(orchCfg, a.Router, a.Registry, a.MemSystem, a.Compressor, a.Emitter)
	a.Recorder = tools.NewChangeRecorder()
	a.Kernel.SetChangeRecorder(a.Recorder)

	// Inject the on-demand local-model loader so /local force / on and the GUI
	// toggle can start the embedded model at runtime (embedded loading lives
	// in package main; the kernel can't import it directly). See
	// Kernel.ApplyLocalPreference.
	a.Kernel.SetLocalLoader(a.loadLocalLLMOnDemand)

	// Persist the cascade rung log next to the rest of memory so the
	// threshold-calibration dataset (which rung answered what, and which
	// local answers the user rejected by re-asking) survives restarts.
	a.Kernel.SetCascadeLogPath(filepath.Join(memDir, "cascade_log.jsonl"))

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
