package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/darkcode/config"
	"github.com/darkcode/core"
	"github.com/darkcode/ingest"
	"github.com/darkcode/intelligence"
	"github.com/darkcode/llm"
	"github.com/darkcode/memory"
	"github.com/darkcode/metrics"
	"github.com/darkcode/observability"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/permission"
	"github.com/darkcode/plugin"
	"github.com/darkcode/project"
	"github.com/darkcode/provider/embedded"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
	"github.com/darkcode/uiport"
)

// Server is the HTTP server that serves the web UI and API.
type Server struct {
	cfg       *config.Config
	registry  *tools.Registry
	memSystem *memory.System
	emitter   *ui.EventEmitter
	kernel    *orchestrator.Kernel
	// port is the one door into the kernel, shared with the CLI and ACP
	// surfaces so every request is assembled the same way.
	port        *uiport.Manager
	approver    *permission.ServerApprover
	projects    *project.Store
	sources     *tools.SourceManager
	httpServer  *http.Server
	mu          sync.Mutex
	activeTasks map[string]bool

	activeChatCancel   context.CancelFunc
	activeChatCancelMu sync.Mutex

	// progressCh receives a coalesced signal for every emitted UI event while
	// a chat turn is in flight, feeding that turn's idle deadline. Nil when no
	// turn is running. See progress_deadline.go.
	progressCh chan struct{}
	progressMu sync.Mutex

	// indexes holds one long-lived code index per workspace, each kept fresh
	// by its own file watcher. Built lazily on first use; see projectIndex.
	// ingesters holds the retrieval-side counterpart, chained onto the same
	// watcher — both go stale on the same event, so they share it rather than
	// each polling the tree. Guarded by indexMu.
	indexes   map[string]*intelligence.ProjectIndex
	ingesters map[string]*ingest.Auto
	indexMu   sync.Mutex

	// apiRateLimiter throttles /api/* requests per remote address.
	apiRateLimiter *rateLimiter

	// idempotency de-duplicates /api/chat POSTs carrying an Idempotency-Key.
	idempotency *idempotencyStore

	// activeWorkspace is the directory the chat console's file explorer
	// browses. It is switched automatically when a project is activated
	activeWorkspace string
	activeProject   string // id of the project whose path is the active workspace ("") = none)
	wsMu            sync.RWMutex
	cfgMu           sync.RWMutex // guards s.cfg mutations (Models map, hot-reload)

	SwitchToCLI chan string // Channel to signal main.go to resume CLI and pass active project

	// GUI disconnect detection (Issue #4): when the browser closes, the last
	// SSE connection drops. After a grace period (to survive tab refresh / a
	// transient SSE reconnect), the server signals SwitchToCLI so main.go
	// resumes CLI mode instead of blocking forever. Previously the only way to
	// resume was the explicit "Switch to CLI" button, so closing the browser
	// left the CLI hung on <-SwitchToCLI.
	ResumeOnDisconnect bool
	guiMu              sync.Mutex
	guiGrace           *time.Timer
	sseEverConnected   bool // true once ≥1 SSE client connected this GUI session
}

// NewServer creates a new HTTP server.
func NewServer(cfg *config.Config, registry *tools.Registry, memSystem *memory.System, emitter *ui.EventEmitter, kernel *orchestrator.Kernel, port *uiport.Manager, approver *permission.ServerApprover, projects *project.Store, sources *tools.SourceManager) *Server {
	s := &Server{
		cfg:         cfg,
		registry:    registry,
		memSystem:   memSystem,
		emitter:     emitter,
		kernel:      kernel,
		port:        port,
		approver:    approver,
		projects:    projects,
		sources:     sources,
		activeTasks: make(map[string]bool),
		SwitchToCLI: make(chan string, 1),
		// 10 requests/sec sustained, burst of 30 — generous for a single local
		// UI session's normal traffic, but bounds a runaway/malicious client.
		apiRateLimiter: newRateLimiter(10, 30),
		// A chat turn can run up to 5 minutes; keep replayable results a bit
		// longer so a retry after a slow response still de-duplicates.
		idempotency: newIdempotencyStore(10 * time.Minute),
	}
	// Register the progress heartbeat once, for the life of the process —
	// per-request registration is unsafe here, see watchProgress.
	s.watchProgress()
	return s
}

// maxChatBodyBytes / maxHTPBodyBytes cap request bodies on the two JSON POST
// endpoints that accept arbitrary user input, so a malicious or buggy client
// can't exhaust memory with an unbounded body.
const maxChatBodyBytes = 10 * 1024 * 1024 // 10 MiB
const maxHTPBodyBytes = 10 * 1024 * 1024  // 10 MiB

// primaryClient builds a fresh LLM client for the currently-configured
// primary model, with the provider set so token/cost telemetry records the
// correct provider. Used by short-lived auxiliary LLM calls in handleChat
// (auto-classifier, project plan/workflow updater) that don't go through the
// router but should still report accurate metrics. Reads s.cfg under the
// config lock so it stays consistent with concurrent /api/config writes.
func (s *Server) primaryClient() core.LLMClient {
	c, _ := s.primaryClientModel()
	return c
}

// primaryClientModel returns the primary aux client AND its model id. The
// model id must be threaded onto requests: the router's wrapped client and
// the local embedded client don't all bake in a default model, so an aux
// request that omits Model triggers a provider "model is not specified"
// error. Callers building their own CompletionRequest should set req.Model
// to the returned id.
func (s *Server) primaryClientModel() (core.LLMClient, string) {
	if s.kernel != nil {
		if c, model, err := s.kernel.PlannerClient(); err == nil && c != nil {
			return c, model
		}
	}
	s.cfgMu.RLock()
	baseURL, apiKey, model, provider := s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model, s.cfg.Provider
	enableLocal := s.cfg.LocalEnabled()
	s.cfgMu.RUnlock()
	if model == "" && baseURL == "" && enableLocal {
		if emb := embedded.Default(); emb != nil {
			if st := emb.Status(); st.State == embedded.StateRunning && st.BaseURL != "" {
				if id := emb.LoadedModelID(); id != "" {
					c := llm.NewClient(st.BaseURL, "no-key-required", id)
					c.SetProvider("embedded")
					c.SetAuthScheme("none")
					return c, id
				}
			}
		}
	}
	c := llm.NewClient(baseURL, apiKey, model)
	c.SetProvider(provider)
	return c, model
}

// shortContinuationMaxLen bounds what counts as a "bare continuation"
// ("continue", "yes", "go on") for needsPlanAmend — long enough to cover
// short acknowledgements, short enough that a real (if terse) instruction
// still triggers a real amend.
const shortContinuationMaxLen = 30

// workflowTaskLineRe matches a workflow checklist line, capturing the
// checkbox state and the task ID — the read side of the same "- [ ] T1: ..."
// format project.Store.MarkTaskStatus writes.
var workflowTaskLineRe = regexp.MustCompile(`^\s*-\s*\[([ x/])\]\s*([A-Za-z0-9_-]+):`)

// mermaidFenceRe locates a ```mermaid ... ``` fenced code block in plan
// markdown (non-greedy, spans newlines).
var mermaidFenceRe = regexp.MustCompile("(?s)```mermaid\\n(.*?)```")

// Handler builds the fully routed, fully wrapped HTTP handler.
//
// Split out of Start so the routing table can be exercised without binding a
// port. Start used to be the only place routes existed, which meant nothing
// could assert what the router actually does with a path — and routing bugs
// are precisely the kind that look fine until a caller hits the wrong one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.Handle("/api/chat", s.csrfMiddleware(s.idempotencyMiddleware(http.HandlerFunc(s.handleChat))))
	mux.Handle("/api/chat/cancel", s.csrfMiddleware(http.HandlerFunc(s.handleCancelChat)))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/verbs", s.handleVerbs)
	mux.HandleFunc("/api/config/schema", s.handleConfigSchema)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/execute", s.handleToolExecute)
	mux.HandleFunc("/api/memory", s.handleMemory)
	mux.HandleFunc("/api/memory/short-term", s.handleShortTermMemory)
	mux.HandleFunc("/api/memory/episodic", s.handleEpisodicMemory)
	mux.HandleFunc("/api/memory/semantic", s.handleSemanticMemory)
	mux.HandleFunc("/api/memory/procedural", s.handleProceduralMemory)
	mux.HandleFunc("/api/events", s.handleSSE) // SSE streaming
	mux.HandleFunc("/api/events/history", s.handleEventHistory)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/ingest", s.handleIngest)
	mux.HandleFunc("/api/session/state", s.handleSessionState)
	mux.HandleFunc("/api/switch-cli", s.handleSwitchCLI)

	// LLM provider catalogue + usage telemetry
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/providers", s.handleProviders)
	mux.HandleFunc("/api/models/fetch", s.handleModelsFetch)
	mux.HandleFunc("/api/models/ping", s.handleModelsPing)
	mux.HandleFunc("/api/models/disable", s.handleModelsDisable)
	mux.HandleFunc("/api/models/enable", s.handleModelsEnable)
	mux.HandleFunc("/api/metrics/tokens", s.handleMetricsTokens)
	mux.HandleFunc("/api/metrics/requests", s.handleMetricsRequests)
	mux.HandleFunc("/api/analytics/history", s.handleMetricsRequests) // Alias for Observability
	mux.HandleFunc("/api/metrics/reset", s.handleMetricsReset)
	mux.HandleFunc("/api/cascade", s.handleCascade)
	mux.HandleFunc("/api/capability", s.handleCapability)

	// OpenAI-compatible surface: any client that speaks that wire format can
	// point its base_url here and use DarkCode as the model.
	mux.HandleFunc("/v1/models", s.handleOpenAIModels)
	mux.HandleFunc("/v1/chat/completions", s.handleOpenAIChat)

	// Architecture extensions
	mux.HandleFunc("/api/checkpoints", s.handleCheckpoints)
	mux.HandleFunc("/api/checkpoints/diff", s.handleCheckpointDiff)
	mux.HandleFunc("/api/rollback", s.handleRollback)
	mux.HandleFunc("/api/runs", s.handleRunEvents)
	mux.HandleFunc("/api/plan", s.handlePlan)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/audit/recent", s.handleAuditRecent)
	mux.HandleFunc("/api/audit/export", s.handleAuditExport)
	mux.HandleFunc("/api/knowledge", s.handleKnowledgeGraph)
	mux.HandleFunc("/api/learning/stats", s.handleLearningStats)
	mux.HandleFunc("/api/agents/messages", s.handleAgentMessages)
	mux.HandleFunc("/api/system/resources", s.handleSystemResources)
	mux.HandleFunc("/api/intelligence/summary", s.handleIntelligenceSummary)

	// Workspace file browser (chat console live directory view)
	mux.HandleFunc("/api/files/list", s.handleFilesList)
	mux.HandleFunc("/api/files/read", s.handleFilesRead)

	// Filesystem directory browser (for directory picker in projects).
	mux.HandleFunc("/api/fs/browse", s.handleFSBrowse)
	mux.HandleFunc("/api/fs/mkdir", s.handleFSMkdir)

	// Workspace — the directory the chat console's file explorer browses.
	// Switched automatically when a project is activated, or manually via
	// the file explorer header button.
	mux.HandleFunc("/api/workspace", s.handleWorkspace)
	mux.HandleFunc("/api/workspace/browse", s.handleWorkspaceBrowse)

	// MCP protocol endpoint
	mux.HandleFunc("/api/mcp", s.handleMCP)

	// Permission system: list pending approval requests + resolve them.
	// The web UI pops up a dialog when an approval "request" SSE event arrives.
	mux.HandleFunc("/api/approvals", s.handleApprovals)
	mux.HandleFunc("/api/approvals/decide", s.handleApprovalDecide)

	// Projects: long-lived per-project context. The web UI exposes a Projects
	// tab for browsing, creating, and editing project notes/context.
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/projects/", s.handleProjectItem)

	// DarkCode Tool Protocol (HTP) endpoint
	mux.HandleFunc("/api/htp", s.handleHTP)

	mux.HandleFunc("/api/plugins", func(w http.ResponseWriter, r *http.Request) {
		pluginDir := filepath.Join(s.activeWorkspace, "plugins")
		if s.activeWorkspace == "" {
			cwd, _ := os.Getwd()
			pluginDir = filepath.Join(cwd, "plugins")
		}

		reg := plugin.NewRegistry(pluginDir)
		_ = reg.DiscoverAll()

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"plugins": reg.Plugins(),
		})
	})

	s.cfgMu.RLock()
	debugPprof := s.cfg.DebugPprof
	s.cfgMu.RUnlock()
	if debugPprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	// Tool Sources — connect/disconnect MCP servers and in-house ITF tools
	// at runtime. Registered before the catch-all "/" so the more specific
	// /api/tools/sources/* patterns take precedence.
	mux.HandleFunc("/api/tools/sources", s.handleToolSources)
	mux.HandleFunc("/api/tools/sources/", s.handleToolSourceItem)

	// An unrouted /api/ path is a client error and must say so.
	//
	// Without this it fell through to the SPA handler below and came back as
	// 200 text/html — a caller asking for a misspelled or removed endpoint got
	// the app shell and a status that claims success, then failed somewhere
	// downstream in JSON.parse with nothing pointing at the real cause. The
	// SPA fallback exists for client-side routes; /api/ is not one.
	//
	// ServeMux prefers the longest matching pattern, so every registered
	// /api/... route above still wins over this.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	})

	// Web UI — embedded single-page frontend served at "/".
	// Registered last; ServeMux gives precedence to the more specific
	// /api/* patterns above, so the UI only catches non-API paths.
	mux.Handle("/", webHandler())

	// CORS + security-headers middleware. The server binds to 127.0.0.1, but
	// these headers are cheap defense-in-depth: nosniff stops MIME-sniff XSS,
	// DENY stops clickjacking, and the referrer policy limits leaking the
	// loopback URL (and any query tokens) in cross-origin referrer headers.
	return s.securityHeaders(s.csrfMiddleware(s.corsMiddleware(s.rateLimitMiddleware(mux))))
}

// Start launches the HTTP server on the configured address.
func (s *Server) Start(addr string) error {
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // no timeout for SSE
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("[server] DarkCode web UI starting on http://%s", addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	// Stop the workspace watchers first: they poll on their own goroutines and
	// would otherwise keep re-parsing files after the server is gone.
	s.stopIndexes()
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes an error JSON response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// skipDirs are directory names excluded from workspace listings (file
// explorer + attachment picker) to keep the tree small and relevant.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "__pycache__": true, ".cache": true,
}

// handleHealth is a simple health check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"version": core.VersionOrDev(),
		"time":    time.Now().Format(time.RFC3339),
	})
}

// handleStatus returns the system status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	entries := s.registry.AllEntries()
	toolList := make([]map[string]interface{}, len(entries))
	for i, te := range entries {
		toolList[i] = map[string]interface{}{
			"name":        te.Name,
			"description": te.Description,
			"category":    te.Category,
			"source":      te.Source,
		}
	}

	var memTypes []string
	if s.memSystem != nil {
		memTypes = []string{"short_term", "episodic", "semantic", "procedural"}
	}

	var skillCount, episodeCount int
	if s.memSystem != nil {
		skillCount = len(s.memSystem.ProceduralAll())
		episodeCount = len(s.memSystem.EpisodicGet())
	}

	var sourceCount, sourceConnected int
	if s.sources != nil {
		for _, src := range s.sources.List() {
			sourceCount++
			if src.Status == "connected" {
				sourceConnected++
			}
		}
	}

	s.cfgMu.RLock()
	model, provider, baseURL := s.cfg.Model, s.cfg.Provider, s.cfg.BaseURL
	routingMode, safetyLevel := s.cfg.RoutingMode, s.cfg.SafetyLevel
	uiMode, maxTurns := s.cfg.UIMode, s.cfg.MaxTurns
	s.cfgMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":                  model,
		"provider":               provider,
		"base_url":               baseURL,
		"routing_mode":           routingMode,
		"safety_level":           safetyLevel,
		"ui_mode":                uiMode,
		"max_turns":              maxTurns,
		"workspace":              s.ActiveWorkspace(),
		"tools":                  toolList,
		"tool_count":             len(toolList),
		"tool_source_count":      sourceCount,
		"tool_sources_connected": sourceConnected,
		"memory_types":           memTypes,
		"skill_count":            skillCount,
		"episode_count":          episodeCount,
		"layers": []string{
			"orchestration_kernel",
			"model_router",
			"compression_agent",
			"memory_system",
			"sub_agents",
			"tool_runtime",
		},
		"hardware": observability.GetHardwareStats(),
		"embedded": s.embeddedStatus(),
		"metrics":  metrics.Default.Snapshot(),
	})
}

// handleChat processes a chat request via the orchestrator kernel.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// ensure core import is used
var _ = core.EventTaskUpdate

// handleAudit returns the full audit log.
func parentDir(p string) string {
	d := filepath.Dir(p)
	if d == p {
		return ""
	}
	return d
}

func extOr(isDir bool, name string) string {
	if isDir {
		return ""
	}
	return filepath.Ext(name)
}
