//go:build !llamacpp

package embedded

// embedded_stub.go — the default (no-CGo) embedded provider.
//
// Previously this was a dead stub: IsAvailable() always returned false and
// CreateClient returned nil. It is now a real, functional provider that:
//   1. Checks whether the llama-server binary is available (PATH or binaryDir)
//   2. Spawns it via ProcessManager when a model is loaded
//   3. Proxies chat completions through llama-server's OpenAI-compatible API
//
// This lets DarkCode run fully offline (spec §3: "Local-First Execution")
// without requiring CGo / native llama.cpp bindings — just the llama-server
// binary on the system. If the binary isn't present, the provider reports
// unavailable and the router falls back to other providers.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/capability"
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/observability"
	"github.com/darkcode/provider"
)

// Provider is the subprocess-backed embedded (local llama.cpp) provider.
//
// There is exactly one shared Provider instance per process (see Default /
// Configure) because llama-server manages a single long-lived OS process and
// one loaded model; creating separate Provider instances would each spawn
// their own ProcessManager — losing track of the running server and risking
// multiple concurrent llama-server processes on different ports.
type Provider struct {
	mu            sync.Mutex
	scheduler     interface{} // kept for API compat; not used in subprocess mode
	pm            *ProcessManager
	modelsDir     string
	binaryDir     string
	loadedModel   string // resolved filesystem path of the running model
	loadedModelID string // original caller ref (e.g. "embedded/foo.gguf") for client/router naming
	caps          *capability.SystemCapabilities // optional; drives GPU offload
	// contextSizeOverride is a user-configured context-window override (from
	// config.embedded_context_size). 0 = auto (RAM-aware default from
	// computeLaunchOpts). >0 = always use this value (wins over the RAM guard).
	contextSizeOverride int
	// loadedContextSize is the actual -c value the currently-running
	// llama-server was launched with (computeLaunchOpts' resolved value, not
	// the override alone — RAM-guard/catalog-native logic may adjust it).
	// Read by EmbeddedClient.ModelInfo() so context-budget decisions (e.g.
	// the ctxengine integration) see the model's real window instead of a
	// guess. 0 before any model has loaded.
	loadedContextSize int
	// generation is bumped every time LoadModel swaps the running model. An
	// in-flight EmbeddedClient captures the generation at creation and fails
	// fast (errModelSwapped) if it changes mid-request, preventing a 60s
	// retry-storm when a model is hot-swapped under an active completion.
	generation uint64

	// lastUsedUnixNano is bumped by EmbeddedClient on every request (see
	// client.go's touch()). Used by the idle-unload monitor to decide when
	// the model has been inactive long enough to free RAM/VRAM.
	lastUsedUnixNano int64
	// idleTimeout: 0 = idle-unload disabled (default — a silently unloaded
	// model means the next request pays a full reload, which most users
	// would not expect without opting in). >0 = unload after this much
	// inactivity. Set via SetIdleTimeout, plumbed from
	// config.embedded_idle_timeout_minutes.
	idleTimeout time.Duration
	// idleStop is closed to stop the current idle-monitor goroutine when a
	// new timeout is configured or the provider is closed. nil when no idle
	// monitor is running.
	idleStop chan struct{}
}

// Singleton plumbing.
var (
	defaultOnce sync.Once
	defaultProv *Provider
)

// Default returns the process-wide embedded provider singleton. The first
// call lazily constructs it (with empty dirs); call Configure to set the
// models/binary directories. All wiring layers (startup, hot-reload, the
// model-list handler) MUST go through here so they share one ProcessManager.
func Default() *Provider {
	defaultOnce.Do(func() {
		defaultProv = &Provider{pm: NewProcessManager("")}
		defaultProv.pm.SetOnCrash(defaultProv.attemptRestart)
	})
	return defaultProv
}

// Configure sets the models/binary directories on the singleton. Empty
// values are ignored, so a read-only caller (e.g. the model-list handler that
// only knows the models dir) cannot clobber a binary directory that the
// startup wiring discovered via the auto-download. Returns the singleton for
// chaining.
func Configure(modelsDir, binaryDir string) *Provider {
	p := Default()
	p.mu.Lock()
	defer p.mu.Unlock()
	if modelsDir != "" {
		p.modelsDir = modelsDir
	}
	if binaryDir != "" {
		p.binaryDir = binaryDir
		p.pm.SetBinaryDir(binaryDir)
	}
	return p
}

// SetCapabilities installs the detected system capabilities on the singleton
// so LoadModel can compute a GPU layer count (-ngl) for the launch. Passing
// nil disables GPU offload (CPU-only), which is the safe default.
func SetCapabilities(caps *capability.SystemCapabilities) {
	p := Default()
	p.mu.Lock()
	p.caps = caps
	p.mu.Unlock()
}

// SetContextSizeOverride sets a user-configured context-window override on
// the singleton. 0 = auto (RAM-aware default from computeLaunchOpts). >0 =
// always use this value, winning over the RAM guard. Plumbed from
// config.embedded_context_size in app_wireup.
func SetContextSizeOverride(n int) {
	p := Default()
	p.mu.Lock()
	p.contextSizeOverride = n
	p.mu.Unlock()
}

// idleCheckInterval is how often the idle monitor polls last-used time.
const idleCheckInterval = 1 * time.Minute

// SetIdleTimeout configures automatic unloading of the local model after the
// given period of inactivity (0 disables it — the default). Safe to call at
// any time, including while a model is loaded; the monitor picks up the new
// timeout on its next tick and a change of timeout restarts the monitor
// goroutine so the new value takes effect immediately.
func SetIdleTimeout(d time.Duration) {
	p := Default()
	p.mu.Lock()
	p.idleTimeout = d
	if p.idleStop != nil {
		close(p.idleStop)
		p.idleStop = nil
	}
	if d > 0 {
		stop := make(chan struct{})
		p.idleStop = stop
		go p.idleMonitor(stop)
	}
	p.mu.Unlock()
}

// touch records that the model was just used, resetting the idle clock. Called
// by EmbeddedClient on every request.
func (p *Provider) touch() {
	atomic.StoreInt64(&p.lastUsedUnixNano, time.Now().UnixNano())
}

// idleMonitor unloads the model after idleTimeout of inactivity. It exits
// when stop is closed (SetIdleTimeout changed/disabled the timeout, or a
// manual LoadModel/UnloadModel happened — either way the next SetIdleTimeout
// or LoadModel call is responsible for restarting it if still wanted).
func (p *Provider) idleMonitor(stop chan struct{}) {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			timeout := p.idleTimeout
			loaded := p.loadedModel != ""
			modelID := p.loadedModelID
			p.mu.Unlock()
			if timeout <= 0 || !loaded {
				continue
			}
			last := atomic.LoadInt64(&p.lastUsedUnixNano)
			if last == 0 || p.pm.Status().State != StateRunning {
				continue
			}
			if time.Since(time.Unix(0, last)) >= timeout {
				observability.Log().Info("embedded model idle-unload", map[string]interface{}{"model": modelID, "idle_for": time.Since(time.Unix(0, last)).String()})
				p.UnloadModel()
			}
		}
	}
}

// attemptRestart is installed as the ProcessManager's onCrash callback. It
// retries launching the last-loaded model a bounded number of times with
// backoff; if all attempts fail, it gives up and leaves the process in
// StateFailed (surfaced to the UI/status endpoints as usual — no silent
// infinite retry loop).
func (p *Provider) attemptRestart() {
	p.mu.Lock()
	modelPath := p.loadedModel
	modelID := p.loadedModelID
	p.mu.Unlock()
	if modelPath == "" {
		return
	}

	observability.Log().Warn("embedded model appears to have crashed; attempting restart", map[string]interface{}{"model": modelID})
	// Bump generation immediately so any client created or in-flight during
	// the restart window fails fast instead of hitting the dead port.
	atomic.AddUint64(&p.generation, 1)

	backoffs := []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}
	for attempt := 0; attempt < len(backoffs); attempt++ {
		if attempt > 0 {
			time.Sleep(backoffs[attempt-1])
		}
		opts := p.computeLaunchOpts(modelPath)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err := p.pm.Start(ctx, modelPath, opts)
		cancel()
		if err == nil {
			// New process, new generation — clients created from here on
			// are valid again.
			atomic.AddUint64(&p.generation, 1)
			p.mu.Lock()
			p.loadedContextSize = opts.ContextSize
			p.mu.Unlock()
			observability.Log().Info("embedded model restarted successfully", map[string]interface{}{"model": modelID, "attempt": attempt + 1})
			p.warmup(p.pm.BaseURL(), modelID)
			return
		}
		observability.Log().Warn("embedded model restart attempt failed", map[string]interface{}{"model": modelID, "attempt": attempt + 1, "error": err.Error()})
	}
	observability.Log().Error("embedded model restart exhausted all attempts; giving up", nil, map[string]interface{}{"model": modelID})
}

// warmup sends a minimal completion request to the freshly-started server so
// the first real user request doesn't pay for cache/JIT warm-up. Best-effort
// and asynchronous — a failure here doesn't affect LoadModel's success, since
// the /health check already confirmed the server is up.
func (p *Provider) warmup(baseURL, modelID string) {
	if baseURL == "" {
		return
	}
	go func() {
		c := llm.NewClient(baseURL, "no-key-required", modelID)
		c.AuthScheme = "none"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		maxTok := 1
		_, err := c.ChatCompletion(ctx, &core.CompletionRequest{
			Messages:  []core.Message{{Role: core.RoleUser, Content: "hi"}},
			MaxTokens: &maxTok,
		})
		if err != nil {
			observability.Log().Warn("embedded model warmup failed (non-fatal)", map[string]interface{}{"model": modelID, "error": err.Error()})
		}
	}()
}

// LoadedModelID returns the original model reference (e.g.
// "embedded/foo.gguf") of the currently loaded model ("" if none). Used by
// the server/orchestrator layers to build a client and register the model
// in the router using the same name the startup wiring used, so hot-reload
// doesn't create a duplicate allModels entry.
func (p *Provider) LoadedModelID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadedModelID
}

// NewProvider creates an embedded provider. The scheduler param is retained
// for API compatibility but the subprocess path does not need it. Returns the
// shared singleton so all callers observe one ProcessManager.
func NewProvider(scheduler interface{}) *Provider {
	p := Default()
	p.mu.Lock()
	p.scheduler = scheduler
	p.mu.Unlock()
	return p
}

// NewProviderWithDirs is the configurable constructor used by the wiring
// layer. It configures the singleton's directories (empty values are ignored
// — see Configure) and returns it. The scheduler param is unused in
// subprocess mode.
func NewProviderWithDirs(scheduler interface{}, modelsDir, binaryDir string) *Provider {
	p := Configure(modelsDir, binaryDir)
	p.mu.Lock()
	p.scheduler = scheduler
	p.mu.Unlock()
	return p
}

func (p *Provider) ID() string   { return "embedded" }
func (p *Provider) Name() string { return "llama.cpp (Embedded)" }

func (p *Provider) Type() provider.ProviderType {
	return provider.ProviderEmbedded
}

// IsAvailable reports whether the llama-server binary is present on the system.
// This is a binary check (no process spawned).
func (p *Provider) IsAvailable(ctx context.Context) bool {
	_, err := p.pm.findBinary()
	return err == nil
}

// ListModels scans the models directory for .gguf files and reports them as
// loadable local models.
func (p *Provider) ListModels(ctx context.Context) ([]core.ModelMetadata, error) {
	if p.modelsDir == "" {
		return []core.ModelMetadata{}, nil
	}
	var models []core.ModelMetadata
	entries, err := os.ReadDir(p.modelsDir)
	if err != nil {
		return nil, fmt.Errorf("read models dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !isGGUF(e.Name()) {
			continue
		}
		info, _ := e.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		models = append(models, core.ModelMetadata{
			ID:        "embedded/" + e.Name(),
			Context:   catalogContextWindow(e.Name()), // native max (0 if unknown)
			SizeBytes: size,
		})
	}
	return models, nil
}

// LoadModel spawns the llama-server process for the given model file.
//
// modelPath may be either a model ID ("embedded/foo.gguf" or bare
// "foo.gguf" as returned by ListModels) or an actual filesystem path; see
// resolveModelPath. This is the root-cause fix for the local-LLM break: the
// previous implementation passed the model ID straight through to llama-server's
// `-m` flag, so the file "embedded/qwen1_5-0_5b-chat-q4_k_m.gguf" was looked up
// relative to the CWD and never found, the process exited immediately, and
// (because early-exit detection was dead code) the caller waited the full 60s
// for a timeout with no stderr.
func (p *Provider) LoadModel(ctx context.Context, modelPath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	resolved, err := p.resolveModelPath(modelPath)
	if err != nil {
		return err
	}

	// Already running the requested model → no-op.
	if p.loadedModel == resolved && p.pm.Status().State == StateRunning {
		return nil
	}
	// A *different* model is running → stop it first so we can swap. The
	// shipped single-model config never hits this, but hot-reload / a model
	// change must be able to switch without "already running" errors.
	if p.loadedModel != "" && p.loadedModel != resolved && p.pm.Status().State == StateRunning {
		p.pm.Stop()
		// Bump generation so in-flight clients fail fast instead of hitting
		// a mid-swap 60s retry-storm.
		atomic.AddUint64(&p.generation, 1)
	}

	opts := p.computeLaunchOpts(resolved)
	if err := p.pm.Start(ctx, resolved, opts); err != nil {
		return err
	}
	p.loadedModel = resolved
	p.loadedModelID = modelPath
	p.loadedContextSize = opts.ContextSize
	atomic.StoreInt64(&p.lastUsedUnixNano, time.Now().UnixNano())
	p.warmup(p.pm.BaseURL(), modelPath)
	return nil
}

// computeLaunchOpts derives GPU offload (-ngl) and context-window (-c)
// settings from the detected capabilities + the model file. The context
// window is ALWAYS set (≥ 32768 when possible, per the user requirement) —
// previously it was left at 0, which made Start() default to 4096.
//
// Context-size policy (in priority order):
//  1. p.contextSizeOverride > 0 → always use it (user config wins).
//  2. RAM guard: < 4GB → 16384; 4–8GB → 32768; ≥ 8GB → the model's catalog
//     native context (32768 for 0.5B–3B, 131072 for 7B).
//  3. No caps (capability detection didn't run) → 32768 (the user minimum;
//     llama-server will fail fast if it doesn't fit, with stderr captured).
//
// GPU offload (-ngl) is conservative: 0 (CPU-only) whenever the GPU vendor
// is unsupported by llama.cpp or the per-layer estimate is unreliable.
func (p *Provider) computeLaunchOpts(modelPath string) LaunchOpts {
	opts := LaunchOpts{}

	// --- Context window (always set) ---
	filename := filepath.Base(modelPath)
	catalogCtx := catalogContextWindow(filename)
	if catalogCtx == 0 {
		catalogCtx = 32768 // unknown model → user's minimum
	}
	if p.contextSizeOverride > 0 {
		opts.ContextSize = p.contextSizeOverride
	} else if p.caps == nil {
		opts.ContextSize = 32768
	} else {
		ramGB := float64(p.caps.Memory.TotalBytes) / float64(1024*1024*1024)
		switch {
		case ramGB < 4:
			opts.ContextSize = 16384 // protect minimal systems (still 4× old default)
		case ramGB < 8:
			opts.ContextSize = 32768 // user's minimum
		default:
			opts.ContextSize = catalogCtx // full native (32768 or 131072)
		}
		// Never exceed the model's native context (llama-server would reject it).
		if opts.ContextSize > catalogCtx && catalogCtx > 0 {
			opts.ContextSize = catalogCtx
		}
	}

	// --- GPU offload (conservative) ---
	if p.caps == nil {
		return opts
	}
	// Only enable GPU offload for vendors llama.cpp reliably ships backends
	// for. Intel iGPUs, for instance, frequently lack a working backend and
	// -ngl would cause a start failure.
	switch p.caps.GPU.Vendor {
	case capability.GPUVendorNvidia, capability.GPUVendorAMD, capability.GPUVendorApple:
		// supported
	default:
		return opts
	}
	vram := p.caps.GPU.VRAMBytes
	if vram == 0 {
		return opts
	}
	fi, err := os.Stat(modelPath)
	if err != nil {
		return opts
	}
	modelBytes := uint64(fi.Size())
	if modelBytes == 0 {
		return opts
	}
	const perLayerBytes = 40 * 1024 * 1024 // ~40MB/layer for q4_k_m GGUF
	numLayers := modelBytes / perLayerBytes
	if numLayers == 0 {
		return opts
	}
	// Offload as many layers as fit in 90% of VRAM (leave headroom for KV
	// cache + activation memory).
	fitByVRAM := (uint64(float64(vram) * 0.9)) / perLayerBytes
	ngl := int(numLayers)
	if fitByVRAM < uint64(ngl) {
		ngl = int(fitByVRAM)
	}
	if ngl < 1 {
		return opts
	}
	if ngl > 999 {
		ngl = 999 // llama-server's practical -ngl ceiling
	}
	opts.NGPULayers = ngl
	return opts
}

// Generation returns the current model generation counter. Used by
// EmbeddedClient to detect a mid-request model swap.
func (p *Provider) Generation() uint64 {
	return atomic.LoadUint64(&p.generation)
}

// ContextSize returns the -c value the currently-running llama-server was
// actually launched with (0 if no model has loaded yet). Used by
// EmbeddedClient.ModelInfo() so callers that budget context (e.g. the
// ctxengine integration) see the model's real window instead of a guess.
func (p *Provider) ContextSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadedContextSize
}

// resolveModelPath turns a model reference into an absolute, verified file
// path. Accepts:
//   - a ListModels ID like "embedded/foo.gguf" → strips the "embedded/"
//     prefix and joins modelsDir;
//   - a bare filename "foo.gguf" → joins modelsDir;
//   - an existing filesystem path → used as-is (pass-through).
// Returns a clear error naming both attempted locations when the file is
// missing, so a bad model dir or typo is obvious instead of manifesting as a
// 60s health-timeout.
func (p *Provider) resolveModelPath(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("embedded: empty model reference")
	}
	// Pass-through: caller already gave a real path.
	if fi, err := os.Stat(ref); err == nil && !fi.IsDir() {
		return ref, nil
	}
	// ID form: strip a leading "embedded/" prefix (as produced by ListModels).
	name := ref
	name = strings.TrimPrefix(name, "embedded/")
	name = strings.TrimPrefix(name, "embedded") // defensive
	name = strings.TrimPrefix(name, "/")
	if p.modelsDir != "" {
		candidate := filepath.Join(p.modelsDir, name)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, nil
		}
		return "", fmt.Errorf("embedded: model file not found: %s (looked for %s and %s)",
			ref, ref, candidate)
	}
	return "", fmt.Errorf("embedded: modelsDir not configured and %q is not an existing path", ref)
}

// UnloadModel stops the embedded server.
func (p *Provider) UnloadModel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pm.Stop()
	p.loadedModel = ""
	p.loadedModelID = ""
}

// CreateClient returns an OpenAI-compatible client that proxies to the local
// llama-server instance. The caller must have loaded a model first.
func (p *Provider) CreateClient(modelID string, opts provider.ClientOptions) (core.LLMClient, error) {
	baseURL := p.pm.BaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("no embedded model loaded; call LoadModel first")
	}
	// llama-server's OpenAI endpoint needs no API key; use a dummy.
	return NewEmbeddedClient(baseURL, "embedded", modelID), nil
}

// Status exposes the underlying process status for the UI / diagnostics.
func (p *Provider) Status() ProcessStatus {
	return p.pm.Status()
}

// Close stops any running server.
func (p *Provider) Close() error {
	p.UnloadModel()
	return nil
}

// isGGUF reports whether name has a .gguf extension (case-insensitive).
func isGGUF(name string) bool {
	if len(name) < 5 {
		return false
	}
	ext := name[len(name)-5:]
	return ext == ".gguf" || ext == ".GGUF"
}

// catalogContextWindow looks up a model filename in the download catalog and
// returns its native context window (tokens). Returns 0 if the model isn't in
// the catalog (unknown model → let the caller use its own default).
func catalogContextWindow(filename string) int {
	for _, m := range modelCatalog {
		if m.Filename == filename {
			return m.ContextWindow
		}
	}
	return 0
}

// humanSize formats a file size for display.
func humanSize(info os.FileInfo) string {
	if info == nil {
		return "unknown size"
	}
	size := info.Size()
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(size)/(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// Ensure unused imports are referenced in case of future changes.
var _ = time.Second
