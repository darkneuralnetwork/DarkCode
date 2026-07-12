package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/darkcode/compression"
	"github.com/darkcode/capability"
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/provider"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/observability"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/plugin"
	"github.com/darkcode/project"
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
	a.initTools(memDir)
	a.initRouterAndModels()
	a.initKernelAndServer()
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

func (a *AppRunner) initMemoryAndProjects() string {
	a.Registry = tools.NewRegistry()
	a.SourceMgr = tools.NewSourceManager(a.Registry)

	resolveDataDir := func(cfgDir, name string) string {
		cwd, _ := os.Getwd()
		fallback := cwd + "/.darkcode/" + name
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

	// Deterministic toolchain (spec §8) — real ripgrep + go/ast backed tools.
	// Registered separately to avoid an import cycle (the deterministic
	// package imports tools.ToolEntry).
	deterministic.RegisterAll(a.Registry)

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

	// binDir is where the auto-downloaded llama-server binary lives. Computed
	// once here so every embedded-provider call site (createClient below and
	// the auto-load block) configures the shared singleton with the same dir;
	// previously createClient passed "" and could not find the downloaded
	// binary, while the auto-load block passed the real dir — two divergent
	// wirings of the same provider. Now both go through the singleton.
	cwd, _ := os.Getwd()
	binDir := filepath.Join(cwd, ".darkcode", "bin")

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
			embProv := embedded.NewProviderWithDirs(nil, "./models", binDir)

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
	}

	var primaryClient core.LLMClient
	if a.Cfg.Model != "" || !a.Cfg.EnableLocalLLM {
		primaryClient = createClient(a.Cfg.Provider, a.Cfg.BaseURL, a.Cfg.APIKey, a.Cfg.Model)
		tier := core.PrimaryTierForMode(routingMode)
		a.Router.RegisterModel(tier, llm.WithRetry(primaryClient, llm.DefaultRetryOpts), a.Cfg.Model)
		a.Router.MarkPrimary(a.Cfg.Model)
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
		if primaryClient != nil {
			tier := core.PrimaryTierForMode(routingMode)
			a.Router.RegisterModel(tier, llm.WithRetry(primaryClient, llm.DefaultRetryOpts), fallbackModel)
			a.Router.MarkPrimary(fallbackModel)
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

	// --- Auto-load Local LLM (Resource Methodology) ---
	if a.Cfg.EnableLocalLLM {
		observability.Log().Info("initialising the local llm", nil)

		// Auto-Download: Ensure llama-server binary and default model exist
		if err := embedded.EnsureLlamaServer(context.Background(), binDir); err != nil {
			observability.Log().Warn("auto-downloader for llama-server failed", map[string]interface{}{"error": err.Error()})
		}

		if caps, err := capability.Detect(context.Background()); err == nil {
			maxAllowedBytes := int64(float64(caps.Memory.TotalBytes+caps.GPU.VRAMBytes) * 0.60)
			_ = maxAllowedBytes // kept for the model-selection log below

			// Plumb detected capabilities into the embedded provider so LoadModel
			// can compute a GPU layer count (-ngl) for supported GPUs.
			embedded.SetCapabilities(caps)
			// Plumb the user's context-window override (0 = auto). When set, the
			// embedded provider always launches with this -c value, winning over
			// the RAM-aware default.
			embedded.SetContextSizeOverride(a.Cfg.EmbeddedContextSize)
			// Plumb the user's idle-unload timeout (0 = disabled, the default).
			if a.Cfg.EmbeddedIdleTimeoutMinutes > 0 {
				embedded.SetIdleTimeout(time.Duration(a.Cfg.EmbeddedIdleTimeoutMinutes) * time.Minute)
			}

			// Auto-Download: Ensure at least one model exists. The function
			// now selects a model appropriate for this system's resources
			// (different RAM/GPU tiers get different-sized models).
			if err := embedded.EnsureDefaultModels(context.Background(), "./models", int64(caps.Memory.TotalBytes), int64(caps.GPU.VRAMBytes)); err != nil {
				observability.Log().Warn("auto-downloader for default models failed", map[string]interface{}{"error": err.Error()})
			}

			embProv := embedded.NewProviderWithDirs(nil, "./models", binDir)
			if models, err := embProv.ListModels(context.Background()); err == nil && len(models) > 0 {
				sort.Slice(models, func(i, j int) bool {
					return models[i].SizeBytes > models[j].SizeBytes
				})
				var selectedMedium, selectedTiny *core.ModelMetadata
				usedBytes := int64(0)
				for i := range models {
					m := models[i]
					if usedBytes+m.SizeBytes <= maxAllowedBytes {
						if selectedMedium == nil {
							selectedMedium = &models[i]
							usedBytes += m.SizeBytes
						} else if selectedTiny == nil {
							selectedTiny = &models[i]
							usedBytes += m.SizeBytes
						}
					}
				}
				
				loadLocal := func(m *core.ModelMetadata, tier core.ModelTier, isPrimary bool) {
					if err := embProv.LoadModel(context.Background(), m.ID); err == nil {
						if c, err := embProv.CreateClient(m.ID, provider.ClientOptions{}); err == nil {
							if emb, ok := c.(*embedded.EmbeddedClient); ok {
								// Wrap emb itself (not emb.Client) so RetryingClient
								// calls through EmbeddedClient's model-swap guard
								// instead of bypassing it via the raw inner client.
								wrapped := llm.WithRetry(emb, llm.DefaultRetryOpts)
								a.Router.RegisterModel(tier, wrapped, m.ID)
								if isPrimary {
									a.Router.RegisterModel(core.PrimaryTierForMode(routingMode), wrapped, m.ID)
									a.Router.MarkPrimary(m.ID)
									if fastClient == nil {
										fastClient = emb
										fastModel = m.ID
										// Compressor was created above with a nil client
										// (no cloud primary, no fast tier yet). Hot-swap it
										// to the now-loaded local model so STM compression
										// runs on the local LLM instead of crashing
										// (error.txt) / heuristic-falling back. Mirrors
										// Kernel.ReloadModels' local-only branch.
										if a.Compressor != nil {
											a.Compressor.SetClient(llm.WithRetry(emb, llm.DefaultRetryOpts), m.ID)
										}
									}
								}
								observability.Log().Info("loaded local model", map[string]interface{}{"model": m.ID, "tier": string(tier)})
							}
						} else {
							observability.Log().Error("error creating client for local model", err, map[string]interface{}{"model": m.ID})
						}
					} else {
						observability.Log().Error("error loading local model", err, map[string]interface{}{"model": m.ID})
					}
				}

				if selectedMedium != nil {
					loadLocal(selectedMedium, core.ModelTierMediumLocal, a.Cfg.Model == "")
				} else if selectedTiny != nil {
					loadLocal(selectedTiny, core.ModelTierTinyLocal, a.Cfg.Model == "")
				}
			} else {
				observability.Log().Warn("could not list local models or no models found", map[string]interface{}{"error": fmt.Sprintf("%v", err)})
			}
		} else {
			observability.Log().Warn("could not detect capabilities for local models", map[string]interface{}{"error": fmt.Sprintf("%v", err)})
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

func (a *AppRunner) initKernelAndServer() {
	routingMode := core.ParseRoutingMode(a.Cfg.RoutingMode)
	orchCfg := orchestrator.DefaultConfig()
	orchCfg.RoutingMode = routingMode
	orchCfg.UIMode = a.Cfg.UIMode
	orchCfg.MaxConcurrent = a.Cfg.MaxConcurrent
	orchCfg.MaxTurns = a.Cfg.MaxTurns
	orchCfg.SafetyLevel = parseSafetyLevel(a.Cfg.SafetyLevel)
	orchCfg.CompressContext = a.Cfg.CompressContext
	orchCfg.UseCtxEngine    = a.Cfg.UseCtxEngine
	orchCfg.AgenticLoop = a.Cfg.AgenticLoop
	orchCfg.MaxLoops = a.Cfg.MaxLoops
	orchCfg.ExecutionProfile = a.Cfg.ExecutionProfile
	orchCfg.ContextLength = a.Cfg.ContextLength

	a.Kernel = orchestrator.New(orchCfg, a.Router, a.Registry, a.MemSystem, a.Compressor, a.Emitter)
	a.Recorder = tools.NewChangeRecorder()
	a.Kernel.SetChangeRecorder(a.Recorder)

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
