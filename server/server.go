package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/darkcode/internal/strutil"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/config"
	"github.com/darkcode/core"
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
)

// Server is the HTTP server that serves the web UI and API.
type Server struct {
	cfg         *config.Config
	registry    *tools.Registry
	memSystem   *memory.System
	emitter     *ui.EventEmitter
	kernel      *orchestrator.Kernel
	approver    *permission.ServerApprover
	projects    *project.Store
	sources     *tools.SourceManager
	httpServer  *http.Server
	mu          sync.Mutex
	activeTasks map[string]bool

	activeChatCancel   context.CancelFunc
	activeChatCancelMu sync.Mutex

	// apiRateLimiter throttles /api/* requests per remote address.
	apiRateLimiter *rateLimiter

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
func NewServer(cfg *config.Config, registry *tools.Registry, memSystem *memory.System, emitter *ui.EventEmitter, kernel *orchestrator.Kernel, approver *permission.ServerApprover, projects *project.Store, sources *tools.SourceManager) *Server {
	s := &Server{
		cfg:         cfg,
		registry:    registry,
		memSystem:   memSystem,
		emitter:     emitter,
		kernel:      kernel,
		approver:    approver,
		projects:    projects,
		sources:     sources,
		activeTasks: make(map[string]bool),
		SwitchToCLI: make(chan string, 1),
		// 10 requests/sec sustained, burst of 30 — generous for a single local
		// UI session's normal traffic, but bounds a runaway/malicious client.
		apiRateLimiter: newRateLimiter(10, 30),
	}
	// Install the workspace resolver so file/terminal/git tools resolve
	// relative paths against the active project's path.
	// We no longer use tools.SetWorkspaceResolver(s.ActiveWorkspace) here,
	// as workspace is now resolved via context.Context per-request.
	return s
}

// buildProjectContextQuery was centralized into project.Store.BuildContextQuery
// so the CLI and server share one summary-aware implementation (the CLI
// previously injected raw context and diverged from the server's
// summary-aware path).

// summaryThreshold is the minimum raw context size (bytes) before the server
// bothers generating a compressed summary. Below this the raw context is small
// enough to inject verbatim.
const summaryThreshold = 12 * 1024 // 12 KiB

// summaryRegrowth is the minimum growth in raw context size (bytes) since the
// last summary generation before the server recompresses. This prevents
// recompressing on every single exchange (cost/latency) while keeping the
// summary reasonably fresh.
const summaryRegrowth = 8 * 1024 // 8 KiB

// maxChatBodyBytes / maxHTPBodyBytes cap request bodies on the two JSON POST
// endpoints that accept arbitrary user input, so a malicious or buggy client
// can't exhaust memory with an unbounded body.
const maxChatBodyBytes = 10 * 1024 * 1024 // 10 MiB
const maxHTPBodyBytes = 10 * 1024 * 1024  // 10 MiB

// maybeRewriteProjectContext uses the local llama-server to rewrite the
// raw context into a few-token compressed version. It overwrites context.md.
func (s *Server) maybeRewriteProjectContext(projID string) {
	if s.projects == nil || strings.TrimSpace(projID) == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[server] project context rewrite panic: %v", r)
		}
	}()
	rawCtx, err := s.projects.GetContext(projID)
	if err != nil {
		return
	}
	if strings.TrimSpace(rawCtx) == "" {
		return
	}
	
	// Force the use of the local embedded model if possible, per user request.
	client := s.primaryClient()
	
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	
	prompt := "Rewrite the following project context into a very concise, few-token briefing that retains all important details.\n\n" + rawCtx
	temp := 0.2
	maxTok := 1024
	req := &core.CompletionRequest{
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "You are a highly efficient context compressor. Rewrite the given text into the fewest possible tokens while keeping all facts, open questions, and important details intact. Output only the compressed text."},
			{Role: core.RoleUser, Content: prompt},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}
	resp, err := client.ChatCompletion(ctx, req)
	if err != nil || len(resp.Choices) == 0 {
		log.Printf("[server] context rewrite failed: %v", err)
		return
	}
	
	rewritten := strings.TrimSpace(resp.Choices[0].Message.Content)
	if rewritten == "" {
		return
	}
	
	_ = s.projects.SetContext(projID, rewritten)
	if s.emitter != nil {
		s.emitter.EmitTaskUpdate("summary_updated", projID, rewritten)
	}
}

// primaryClient builds a fresh LLM client for the currently-configured
// primary model, with the provider set so token/cost telemetry records the
// correct provider. Used by short-lived auxiliary LLM calls in handleChat
// (auto-classifier, project plan/workflow updater) that don't go through the
// router but should still report accurate metrics. Reads s.cfg under the
// config lock so it stays consistent with concurrent /api/config writes.
func (s *Server) primaryClient() *llm.Client {
	s.cfgMu.RLock()
	baseURL, apiKey, model, provider := s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model, s.cfg.Provider
	enableLocal := s.cfg.EnableLocalLLM
	s.cfgMu.RUnlock()
	// Local-only setup: no cloud model configured but the embedded llama-server
	// is running. Route the auxiliary calls (auto-classifier, plan/workflow
	// rewriter) through it instead of returning a client with an empty BaseURL
	// — which previously produced silent failures (requests to "/chat/completions"
	// with no host). This mirrors how the router serves the main chat path.
	if model == "" && baseURL == "" && enableLocal {
		if emb := embedded.Default(); emb != nil {
			if st := emb.Status(); st.State == embedded.StateRunning && st.BaseURL != "" {
				if id := emb.LoadedModelID(); id != "" {
					c := llm.NewClient(st.BaseURL, "no-key-required", id)
					c.SetProvider("embedded")
					c.SetAuthScheme("none")
					return c
				}
			}
		}
	}
	c := llm.NewClient(baseURL, apiKey, model)
	c.SetProvider(provider)
	return c
}

// seedProjectPlanWorkflow ensures a freshly created/opened project has a
// non-empty plan + workflow so the GUI "Plan & Workflow" tab never shows the
// "Awaiting plan generation…" placeholder indefinitely. It first writes an
// idempotent skeleton (instant, always succeeds), then kicks off an async
// LLM generation that rewrites the skeleton from the project's description +
// context. If the LLM call fails, the skeleton remains — the tab is still
// populated. This is the root-cause fix for the empty plan/workflow tabs:
// previously generation only ran after the first chat exchange AND only when
// req.Project was non-empty, so a new project sat empty until first chat.
func (s *Server) seedProjectPlanWorkflow(projID, name, description, ctxBody string) {
	if s.projects == nil || strings.TrimSpace(projID) == "" {
		return
	}
	// 1. Instant skeleton seed (idempotent) so the tab is never empty.
	seedPlan, _ := s.projects.EnsurePlanSeeded(projID, "")
	seedWf, _ := s.projects.EnsureWorkflowSeeded(projID, "")
	if s.emitter != nil {
		s.emitter.EmitPlanUpdated(projID, seedPlan)
		s.emitter.EmitWorkflowUpdated(projID, seedWf)
	}
	// 2. Async LLM rewrite from description + context (best-effort).
	go func(projID, name, description, ctxBody string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ctx = context.WithValue(ctx, core.WorkspaceKey, s.ActiveWorkspace())
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[server] plan/workflow seed panic: %v", r)
			}
		}()
		client := s.primaryClient()
		temp := 0.2
		desc := strings.TrimSpace(description)
		if desc == "" {
			desc = name
		}
		ctxNote := ""
		if strings.TrimSpace(ctxBody) != "" {
			ctxNote = "\n\nExisting project context:\n" + strutil.TruncateForPrompt(ctxBody, 4000)
		}
		planPrompt := fmt.Sprintf("You are an AI architect. Generate a comprehensive Implementation Plan in raw markdown for a project named %q.\nDescription: %s%s\n\nInclude sections like Goal Description, Proposed Changes, and Verification Plan. You MUST also include an Architecture section featuring a Mermaid graph (`graph TD`) visualizing the data flow and project architecture. If you have any confusion or underspecified requirements, add an 'Open Questions' section to ask the user.\n\nOutput ONLY the markdown plan, and ensure the mermaid graph is wrapped in ```mermaid ... ```.", name, desc, ctxNote)
		wfPrompt := fmt.Sprintf("You are an AI architect. Generate a concise Task Workflow in raw markdown for a project named %q.\nDescription: %s%s\n\nFormat the workflow strictly as a checklist using GitHub-flavored markdown checkboxes: use \"- [ ]\" for pending steps and \"- [x]\" for completed steps. Group steps under ## phase headings.\n\nOutput ONLY the markdown.", name, desc, ctxNote)
		pResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{Messages: []core.Message{{Role: "system", Content: "You are an AI architect. Keep the plan detailed but concise. You MUST include a Mermaid architecture graph. Only output valid markdown."}, {Role: "user", Content: planPrompt}}, Temperature: &temp})
		if err == nil && len(pResp.Choices) > 0 {
			planText := pResp.Choices[0].Message.Content
			if strings.TrimSpace(planText) != "" {
				s.projects.SetPlan(projID, planText)
				if s.emitter != nil {
					s.emitter.EmitPlanUpdated(projID, planText)
				}
			}
		}
		wResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{Messages: []core.Message{{Role: "system", Content: "You are an AI architect. Keep the workflow architecture concise. Use GitHub-flavored markdown checkboxes (- [ ] / - [x]) for steps. Only output valid markdown."}, {Role: "user", Content: wfPrompt}}, Temperature: &temp})
		if err == nil && len(wResp.Choices) > 0 {
			wfText := wResp.Choices[0].Message.Content
			if strings.TrimSpace(wfText) != "" {
				s.projects.SetWorkflow(projID, wfText)
				if s.emitter != nil {
					s.emitter.EmitWorkflowUpdated(projID, wfText)
				}
			}
		}
	}(projID, name, description, ctxBody)
}

// truncateForPrompt caps a string to ~maxChars for inclusion in an LLM prompt.

// regeneratePlanWorkflow re-runs the async LLM rewrite for the plan and/or
// workflow of a project. kind is "plan", "workflow", or "" (both). It is the
// backend for the Blueprint task board's "Regenerate" button. The rewrite is
// best-effort: errors are logged and the skeleton seed remains.
func (s *Server) regeneratePlanWorkflow(projID, kind string) {
	if s.projects == nil || strings.TrimSpace(projID) == "" {
		return
	}
	p, err := s.projects.GetWithContext(projID)
	if err != nil || p == nil {
		return
	}
	// Re-seed the skeleton immediately (in case plan/workflow files are
	// missing), then fire the async LLM rewrite.
	if kind == "plan" || kind == "" {
		seed, _ := s.projects.EnsurePlanSeeded(projID, "")
		if s.emitter != nil {
			s.emitter.EmitPlanUpdated(projID, seed)
		}
	}
	if kind == "workflow" || kind == "" {
		seed, _ := s.projects.EnsureWorkflowSeeded(projID, "")
		if s.emitter != nil {
			s.emitter.EmitWorkflowUpdated(projID, seed)
		}
	}
	// Delegate the actual LLM rewrite to the existing seed rewriter.
	s.seedProjectPlanWorkflow(projID, p.Name, p.Description, p.Context)
}

// Start launches the HTTP server on the configured address.
func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()

	// API routes
	mux.Handle("/api/chat", s.csrfMiddleware(http.HandlerFunc(s.handleChat)))
	mux.Handle("/api/chat/cancel", s.csrfMiddleware(http.HandlerFunc(s.handleCancelChat)))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/execute", s.handleToolExecute)
	mux.HandleFunc("/api/memory", s.handleMemory)
	mux.HandleFunc("/api/memory/short-term", s.handleShortTermMemory)
	mux.HandleFunc("/api/memory/episodic", s.handleEpisodicMemory)
	mux.HandleFunc("/api/memory/semantic", s.handleSemanticMemory)
	mux.HandleFunc("/api/memory/procedural", s.handleProceduralMemory)
	mux.HandleFunc("/api/skills", s.handleSkills)
	mux.HandleFunc("/api/episodes", s.handleEpisodes)
	mux.HandleFunc("/api/events", s.handleSSE) // SSE streaming
	mux.HandleFunc("/api/events/history", s.handleEventHistory)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/session/state", s.handleSessionState)
	mux.HandleFunc("/api/switch-cli", s.handleSwitchCLI)

	// LLM provider catalogue + usage telemetry
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/providers", s.handleProviders)
	mux.HandleFunc("/api/models/fetch", s.handleModelsFetch)
	mux.HandleFunc("/api/metrics/tokens", s.handleMetricsTokens)
	mux.HandleFunc("/api/metrics/requests", s.handleMetricsRequests)
	mux.HandleFunc("/api/analytics/history", s.handleMetricsRequests) // Alias for Observability
	mux.HandleFunc("/api/metrics/reset", s.handleMetricsReset)
	mux.HandleFunc("/api/capability", s.handleCapability)

	// Architecture extensions
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/audit/recent", s.handleAuditRecent)
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

	// Plugins endpoint (Phase 16)
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

	// Profiler endpoints (Phase 16). Gated behind --debug: pprof leaks
	// process args/env and lets any caller trigger CPU-consuming profile
	// captures, so it must not be registered by default.
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

	// Web UI — embedded single-page frontend served at "/".
	// Registered last; ServeMux gives precedence to the more specific
	// /api/* patterns above, so the UI only catches non-API paths.
	mux.Handle("/", webHandler())

	// CORS + security-headers middleware. The server binds to 127.0.0.1, but
	// these headers are cheap defense-in-depth: nosniff stops MIME-sniff XSS,
	// DENY stops clickjacking, and the referrer policy limits leaking the
	// loopback URL (and any query tokens) in cross-origin referrer headers.
	handler := s.securityHeaders(s.csrfMiddleware(s.corsMiddleware(s.rateLimitMiddleware(mux))))

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // no timeout for SSE
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("[server] DarkCode web UI starting on http://%s", addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// corsMiddleware restricts cross-origin access. The embedded frontend is
// same-origin and needs no CORS headers at all, so we only reflect an Origin
// when it is a localhost origin (for optional local dev frontends). We never
// emit "Access-Control-Allow-Origin: *" — that would let any website issue
// authenticated requests to the local agent (drive-by RCE via /api/tools).
// securityHeaders adds defense-in-depth browser security headers to every
// response. The server is loopback-only, but these are cheap and stop common
// XSS / clickjacking vectors in case the UI is ever proxied or framed.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isLocalhostOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a small, dependency-free per-remote-addr token bucket. It
// protects /api/* from a rogue or buggy client flooding the server (and
// exhausting LLM-provider budgets) since there is no other request
// throttling anywhere in front of the kernel.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

func newRateLimiter(ratePerSecond, burst float64) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    ratePerSecond,
		burst:   burst,
	}
}

// allow reports whether a request from key may proceed, consuming one token
// if so. Stale buckets are not actively swept; the map stays bounded in
// practice by the small number of distinct client addresses a loopback-only
// server ever sees.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	now := time.Now()
	if !ok {
		b = &tokenBucket{tokens: rl.burst - 1, lastFill: now}
		rl.buckets[key] = b
		return true
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimitMiddleware throttles /api/* requests per remote address. health
// checks and SSE are exempt (SSE holds one long-lived connection, not a
// stream of requests to throttle).
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/health" && r.URL.Path != "/api/events" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !s.apiRateLimiter.allow(host) {
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalhostOrigin reports whether an Origin header points at a loopback host.
func isLocalhostOrigin(o string) bool {
	for _, p := range []string{"http://127.0.0.1:", "http://localhost:", "https://127.0.0.1:", "https://localhost:", "http://[::1]:", "https://[::1]:"} {
		if strings.HasPrefix(o, p) {
			return true
		}
	}
	return false
}

// csrfMiddleware blocks drive-by cross-origin requests. The server is always
// bound to 127.0.0.1, so there is no remote attacker and no bearer token is
// needed — but a malicious website (evil.com) open in the user's browser can
// still issue fetch() calls to localhost. Any /api/* request (except
// /api/health) carrying a non-loopback Origin header is rejected.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/health" {
			if origin := r.Header.Get("Origin"); origin != "" && !isLocalhostOrigin(origin) {
				writeError(w, http.StatusForbidden, "blocked: cross-origin requests are not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
		"version": "1.0.0",
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

	// Snapshot the config fields under the read lock. handleStatus previously
	// read s.cfg.Model/Provider/... unlocked, racing with /api/config writes.
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
		"metrics": metrics.Default.Snapshot(),
	})
}

// handleChat processes a chat request via the orchestrator kernel.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func (s *Server) setTaskActive(id string, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active {
		s.activeTasks[id] = true
	} else {
		delete(s.activeTasks, id)
	}
}

func (s *Server) getActiveTasks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]string, 0, len(s.activeTasks))
	for id := range s.activeTasks {
		tasks = append(tasks, id)
	}
	return tasks
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

// activeProjectID returns the id of the currently-active project, if any.
// The active project is tracked by the frontend (localStorage) and re-applied
// on each workspace switch so the server can echo it back in /api/workspace.
func (s *Server) activeProjectID() string {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	return s.activeProject
}

// SetActiveProject records which project currently owns the workspace and
// switches the workspace to that project's path. An empty id clears both.
func (s *Server) SetActiveProject(id string) {
	s.wsMu.Lock()
	s.activeProject = id
	s.wsMu.Unlock()
}

// ============================================================================
// FILESYSTEM BROWSER — for directory picker in project creation
// ============================================================================

// handleFSBrowse lists directories at a given absolute path for the directory
// picker UI. Unlike workspace/browse, this is unrestricted to any workspace and
// only returns directories (not files). Query: ?path=<abs_path>.
func (s *Server) handleSwitchCLI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Non-blocking send to SwitchToCLI
	s.signalSwitchToCLI(req.ProjectID)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "switching"})
}

// signalSwitchToCLI performs a non-blocking send on SwitchToCLI. If main.go
// is not currently blocked on the receive (e.g. not in GUI mode), the signal
// is dropped — this is intentional and avoids blocking the HTTP/SSE path.
func (s *Server) signalSwitchToCLI(projectID string) {
	select {
	case s.SwitchToCLI <- projectID:
	default:
	}
}

// SetGUIActive toggles whether disconnect-driven CLI resume is armed.
// main.go calls this on every CLI↔GUI transition:
//
//   - entering GUI mode  → SetGUIActive(true)  (arm: a browser close resumes CLI)
//   - entering CLI mode  → SetGUIActive(false) (disarm: the user is now driving
//     the terminal; stray SSE disconnects from a leftover browser tab must NOT
//     fire the grace timer and corrupt the readline prompt, and must NOT push
//     a stale SwitchToCLI signal that would later bounce GUI→CLI instantly)
//
// This is the root-cause fix for the ">>> [gui] last SSE client gone…"
// prompt-corruption bug: previously ResumeOnDisconnect stayed true across the
// transition into CLI mode, so a leftover/reopened browser tab could arm the
// grace timer while the CLI owned the terminal.
func (s *Server) SetGUIActive(active bool) {
	s.guiMu.Lock()
	defer s.guiMu.Unlock()
	if active {
		if s.guiGrace != nil {
			s.guiGrace.Stop()
			s.guiGrace = nil
		}
		s.sseEverConnected = false
		s.ResumeOnDisconnect = true
	} else {
		if s.guiGrace != nil {
			s.guiGrace.Stop()
			s.guiGrace = nil
		}
		s.sseEverConnected = false
		s.ResumeOnDisconnect = false
	}
}

// BeginGUISession is retained for compatibility; it is equivalent to
// SetGUIActive(true). New callers should use SetGUIActive.
func (s *Server) BeginGUISession() { s.SetGUIActive(true) }

// onSSEConnect is called when an SSE client connects. It cancels any pending
// disconnect grace timer (e.g. the browser refreshed and reconnected within
// the grace window) and records that the GUI has been used this session.
func (s *Server) onSSEConnect() {
	s.guiMu.Lock()
	defer s.guiMu.Unlock()
	if s.guiGrace != nil {
		s.guiGrace.Stop()
		s.guiGrace = nil
	}
	s.sseEverConnected = true
}

// onSSEDisconnect is called when an SSE client disconnects (browser closed,
// tab navigated away, network drop). If this was the last subscriber and the
// GUI has been used this session, it arms a grace timer; when the timer fires
// (no new client reconnected within the window) it signals main.go to resume
// CLI mode.
func (s *Server) onSSEDisconnect() {
	s.guiMu.Lock()
	if !s.ResumeOnDisconnect || !s.sseEverConnected {
		s.guiMu.Unlock()
		return
	}
	// Re-check the subscriber count under guiMu; if a new client already
	// reconnected there is nothing to do.
	if s.emitter.SubscriberCount() > 0 {
		s.guiMu.Unlock()
		return
	}
	log.Printf("[gui] last SSE client gone; arming %v resume-CLI grace", guiDisconnectGrace)
	if s.guiGrace != nil {
		s.guiGrace.Stop()
	}
	s.guiGrace = time.AfterFunc(guiDisconnectGrace, func() {
		s.guiMu.Lock()
		// Re-check: a client may have reconnected during the grace window.
		if s.emitter.SubscriberCount() > 0 {
			s.guiGrace = nil
			s.guiMu.Unlock()
			return
		}
		s.sseEverConnected = false
		s.guiGrace = nil
		pid := s.activeProjectID()
		log.Printf("[gui] grace fired; resuming CLI project=%q subs=%d", pid, s.emitter.SubscriberCount())
		s.guiMu.Unlock()
		s.signalSwitchToCLI(pid)
	})
	s.guiMu.Unlock()
}

func (s *Server) handleSessionState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_project": s.activeProjectID(),
	})
}
