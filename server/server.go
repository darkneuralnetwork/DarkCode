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
	"regexp"
	"sort"
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

	// Route this rewrite through the kernel's compressor (Part 5b): it already
	// honors the local model via the compressor's useLocal path, so this
	// per-project-turn call runs on the local model at $0 when one is loaded,
	// and on the cloud compressor otherwise — instead of always burning a
	// cloud primary call. nil-safe / fail-quiet.
	if s.kernel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rewritten, err := s.kernel.CompressProjectContext(ctx, rawCtx, projID)
	if err != nil || strings.TrimSpace(rewritten) == "" {
		if err != nil {
			log.Printf("[server] context rewrite failed: %v", err)
		}
		return
	}

	_ = s.projects.SetContext(projID, strings.TrimSpace(rewritten))
	if s.emitter != nil {
		s.emitter.EmitTaskUpdate("summary_updated", projID, strings.TrimSpace(rewritten))
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
		planPrompt := fmt.Sprintf("You are an AI architect. Generate a comprehensive Implementation Plan in raw markdown for a project named %q.\nDescription: %s%s\n\nInclude sections like Goal Description, Proposed Changes, and Verification Plan. You MUST also include an Architecture section featuring a Mermaid graph (`graph TD`) visualizing the data flow and project architecture, where every node ID exactly matches a Workflow task ID (format T1, T2, T3, ...). If you have any confusion or underspecified requirements, add an 'Open Questions' section to ask the user.\n\nOutput ONLY the markdown plan, and ensure the mermaid graph is wrapped in ```mermaid ... ```.", name, desc, ctxNote)
		wfPrompt := fmt.Sprintf("You are an AI architect. Generate a concise Task Workflow in raw markdown for a project named %q.\nDescription: %s%s\n\nFormat every step strictly as \"- [ ] T<n>: <one-line approach>\" (pending) or \"- [x] T<n>: ...\" (done) — assign each task a stable ID (T1, T2, T3, ...) matching the Implementation Plan's Mermaid node IDs. Group steps under ## phase headings.\n\nOutput ONLY the markdown.", name, desc, ctxNote)
		var planText, wfText string
		if pResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{Messages: []core.Message{{Role: "system", Content: "You are an AI architect. Keep the plan detailed but concise. You MUST include a Mermaid architecture graph whose node IDs match the workflow's task IDs (T1, T2, ...). Only output valid markdown."}, {Role: "user", Content: planPrompt}}, Temperature: &temp}); err == nil && len(pResp.Choices) > 0 {
			planText = strings.TrimSpace(pResp.Choices[0].Message.Content)
		}
		if wResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{Messages: []core.Message{{Role: "system", Content: "You are an AI architect. Keep the workflow architecture concise. Use \"- [ ] T<n>: ...\" / \"- [x] T<n>: ...\" checkboxes with stable task IDs. Only output valid markdown."}, {Role: "user", Content: wfPrompt}}, Temperature: &temp}); err == nil && len(wResp.Choices) > 0 {
			wfText = strings.TrimSpace(wResp.Choices[0].Message.Content)
		}
		if planText != "" {
			if wfText != "" {
				planText = injectNodeStatus(planText, wfText)
			}
			s.projects.SetPlan(projID, planText)
			if s.emitter != nil {
				s.emitter.EmitPlanUpdated(projID, planText)
			}
		}
		if wfText != "" {
			s.projects.SetWorkflow(projID, wfText)
			if s.emitter != nil {
				s.emitter.EmitWorkflowUpdated(projID, wfText)
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

// shortContinuationMaxLen bounds what counts as a "bare continuation"
// ("continue", "yes", "go on") for needsPlanAmend — long enough to cover
// short acknowledgements, short enough that a real (if terse) instruction
// still triggers a real amend.
const shortContinuationMaxLen = 30

// needsPlanAmend reports whether query should trigger a synchronous
// plan/workflow rewrite before Execute runs (local-first upgrade §5: "if a
// new instruction comes, first change these, then go through these only").
// Skips the amend — reuse the existing plan/workflow unchanged — only for a
// short continuation after a real prior turn, using the same continuation
// signal the clarification gate uses (orchestrator.HasActiveConversation)
// so both decisions agree on what counts as "just a continuation": a bare
// "continue"/"yes" shouldn't cost an LLM round-trip and shouldn't churn the
// plan, but anything else is plausibly a new instruction and gets a fresh
// amend.
func needsPlanAmend(query string, stm []core.Message, skipReadOnly bool) bool {
	trimmed := strings.TrimSpace(query)
	if orchestrator.HasActiveConversation(stm) && len(trimmed) < shortContinuationMaxLen {
		return false
	}
	// A read-only / question turn ("what does X do?", "explain the plan")
	// can't change the plan, so amending it is 2 wasted cloud calls
	// (Part 5b). Skip when SkipAuxForReadOnly is on.
	if skipReadOnly && orchestrator.QueryIsInformational(query) {
		return false
	}
	return true
}

// amendPlanWorkflowSync synchronously rewrites the plan+workflow for a new
// instruction, BEFORE the kernel executes it — replacing the old design
// where the rewrite ran in a goroutine launched only AFTER Execute returned
// (racing with a separate pre-Execute "mark task running" goroutine), so
// execution always ran against a stale plan. Bounded by a tight sub-timeout
// so a slow LLM can't stall chat; on timeout/error the old plan/workflow are
// returned unchanged (fail-open — matches the existing best-effort
// philosophy of every other project LLM call in this file).
func (s *Server) amendPlanWorkflowSync(ctx context.Context, projID, query, oldPlan, oldWorkflow string) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Prefer the local model for these two rewrites when one is healthy
	// (Part 5b) — else the cloud primary, unchanged. RouteAux returns a
	// core.LLMClient; fall back to primaryClient when no model routes.
	var client core.LLMClient = s.primaryClient()
	if s.kernel != nil {
		if lc, _, ok := s.kernel.RouteAux("plan_amend", 0); ok && lc != nil {
			client = lc
		}
	}
	temp := 0.2

	plan := oldPlan
	planPrompt := fmt.Sprintf("Here is the current Implementation Plan and Architecture:\n%s\n\nThe user just requested: %s\n\nRewrite the implementation plan to reflect this new instruction BEFORE any work is done — describe what will change, not what already happened (the workflow tracks progress separately). You MUST keep the Mermaid architecture graph (```mermaid graph TD ... ```), and every node ID MUST exactly match a Workflow task ID (format T1, T2, T3, ...) — reuse existing IDs for existing tasks and only add new IDs for new tasks. If underspecified, add an 'Open Questions' section. Output ONLY the raw markdown plan.", oldPlan, query)
	if pResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
		Messages: []core.Message{
			{Role: "system", Content: "You are an AI architect. Keep the plan detailed but concise. Only output valid markdown. Always include a Mermaid graph whose node IDs exactly match the workflow's task IDs (T1, T2, ...)."},
			{Role: "user", Content: planPrompt},
		},
		Temperature: &temp,
	}); err == nil && len(pResp.Choices) > 0 {
		if t := strings.TrimSpace(pResp.Choices[0].Message.Content); t != "" {
			plan = t
		}
	}

	workflow := oldWorkflow
	wfPrompt := fmt.Sprintf("Here is the current Task Workflow:\n%s\n\nThe user just requested: %s\n\nRewrite the workflow to reflect this new instruction: adjust or add tasks as needed, and mark the task that will be worked on next as running. Format every line strictly as \"- [ ] T<n>: <one-line approach>\" (pending), \"- [/] T<n>: ...\" (running), or \"- [x] T<n>: ...\" (done). Task IDs (T1, T2, ...) MUST stay stable across rewrites — never renumber an existing task, only add new IDs for genuinely new tasks. Output ONLY the raw markdown checklist.", oldWorkflow, query)
	if wResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
		Messages: []core.Message{
			{Role: "system", Content: "You are an AI architect. Only output valid markdown checklists with stable T<n> task IDs."},
			{Role: "user", Content: wfPrompt},
		},
		Temperature: &temp,
	}); err == nil && len(wResp.Choices) > 0 {
		if t := strings.TrimSpace(wResp.Choices[0].Message.Content); t != "" {
			workflow = t
		}
	}

	// Status-linked flowgraph (local-first upgrade §5): stamp the workflow's
	// real task status onto the plan's Mermaid graph so the architecture
	// diagram is a genuine real-time view, not a static snapshot.
	plan = injectNodeStatus(plan, workflow)

	if projID != "" && s.projects != nil {
		_ = s.projects.SetPlan(projID, plan)
		_ = s.projects.SetWorkflow(projID, workflow)
		if s.emitter != nil {
			s.emitter.EmitPlanUpdated(projID, plan)
			s.emitter.EmitWorkflowUpdated(projID, workflow)
		}
	}
	return plan, workflow
}

// workflowTaskLineRe matches a workflow checklist line, capturing the
// checkbox state and the task ID — the read side of the same "- [ ] T1: ..."
// format project.Store.MarkTaskStatus writes.
var workflowTaskLineRe = regexp.MustCompile(`^\s*-\s*\[([ x/])\]\s*([A-Za-z0-9_-]+):`)

// parseWorkflowTaskStatuses extracts a task-ID → Mermaid classDef name map
// ("done"/"running"/"pending") from a workflow's checklist lines.
func parseWorkflowTaskStatuses(workflow string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(workflow, "\n") {
		m := workflowTaskLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		status := "pending"
		switch m[1] {
		case "x":
			status = "done"
		case "/":
			status = "running"
		}
		out[m[2]] = status
	}
	return out
}

// mermaidFenceRe locates a ```mermaid ... ``` fenced code block in plan
// markdown (non-greedy, spans newlines).
var mermaidFenceRe = regexp.MustCompile("(?s)```mermaid\\n(.*?)```")

// injectNodeStatus appends Mermaid classDef/class styling to the plan's
// mermaid fence so the architecture flowgraph visually reflects the
// workflow's real task status (green=done, amber=running, gray=pending) —
// the "real-time flowgraph which showing whats done and what not" from the
// local-first upgrade plan, built on the already-vendored mermaid.min.js
// (no new diagramming library, no client-side changes needed). No-op if the
// plan has no mermaid fence or the workflow has no ID'd task lines to map.
func injectNodeStatus(plan, workflow string) string {
	statuses := parseWorkflowTaskStatuses(workflow)
	if len(statuses) == 0 {
		return plan
	}
	return mermaidFenceRe.ReplaceAllStringFunc(plan, func(block string) string {
		m := mermaidFenceRe.FindStringSubmatch(block)
		if m == nil {
			return block
		}
		body := strings.TrimRight(m[1], "\n")
		var ids []string
		for id := range statuses {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		var sb strings.Builder
		sb.WriteString("```mermaid\n")
		sb.WriteString(body)
		sb.WriteString("\n")
		sb.WriteString("classDef done fill:#2ea043,stroke:#1a7f37,color:#fff\n")
		sb.WriteString("classDef running fill:#d29922,stroke:#9e6a03,color:#fff\n")
		sb.WriteString("classDef pending fill:#30363d,stroke:#8b949e,color:#c9d1d9\n")
		for _, id := range ids {
			fmt.Fprintf(&sb, "class %s %s\n", id, statuses[id])
		}
		sb.WriteString("```")
		return sb.String()
	})
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
