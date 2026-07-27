package server

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/darkcode/internal/strutil"

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
		// A chat turn can run up to 5 minutes; keep replayable results a bit
		// longer so a retry after a slow response still de-duplicates.
		idempotency: newIdempotencyStore(10 * time.Minute),
	}
	return s
}

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
	enableLocal := s.cfg.EnableLocalLLM
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

// seedProjectPlanWorkflow gives a new project a non-empty plan + workflow so
// the GUI tab is never blank: it writes an idempotent skeleton immediately,
// then kicks off an async LLM rewrite. If the rewrite fails, the skeleton stays.
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
		// One combined call (plan + architecture + workflow) replaces the old
		// two calls — cheaper, and no half-generated state. 60s: the single call
		// does more work than either half did.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		ctx = context.WithValue(ctx, core.WorkspaceKey, s.ActiveWorkspace())
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[server] plan/workflow seed panic: %v", r)
			}
		}()
		client, clientModel := s.primaryClientModel()
		desc := strings.TrimSpace(description)
		if desc == "" {
			desc = name
		}
		ctxNote := ""
		if strings.TrimSpace(ctxBody) != "" {
			ctxNote = "\n\nExisting project context:\n" + strutil.TruncateForPrompt(ctxBody, 4000)
		}
		planText, wfText, genErr := s.generatePlanWorkflow(ctx, client, clientModel, name, desc, ctxNote)
		if planText == "" && wfText == "" {
			// Surface the SPECIFIC failure so the Blueprint tab shows why (e.g.
			// the clean quota message) plus Regenerate, instead of a generic
			// error. The skeleton stays visible underneath.
			reason := "Plan/workflow generation failed — click Regenerate to retry"
			if genErr != nil {
				reason = "Plan/workflow generation failed: " + genErr.Error() + " — click Regenerate to retry"
			}
			log.Printf("[server] plan/workflow generation produced nothing for %s: %v", projID, genErr)
			if s.emitter != nil {
				s.emitter.EmitTaskUpdate("blueprint", "error", reason)
			}
			return
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

// generatePlanWorkflow produces the Implementation Plan (with a mermaid
// architecture graph) AND the Task Workflow in a SINGLE LLM call, split by a
// stable delimiter. Retries once on failure/empty/parse-miss. Returns
// ("","",err) when generation genuinely failed so the caller can surface a
// specific reason (e.g. a clean quota message) instead of a generic error.
func (s *Server) generatePlanWorkflow(ctx context.Context, client core.LLMClient, model, name, desc, ctxNote string) (plan, workflow string, genErr error) {
	temp := 0.2
	sys := "You are an AI software architect. Output ONLY raw markdown, in TWO sections separated by a line containing exactly ===WORKFLOW===\n" +
		"Section 1 = Implementation Plan: Goal Description, Proposed Changes, Verification Plan, and an Architecture section containing a ```mermaid\\ngraph TD``` whose node IDs are T1, T2, T3, ...\n" +
		"Section 2 = Task Workflow: every step as \"- [ ] T<n>: <one-line approach>\" grouped under ## phase headings, with task IDs T1, T2, ... matching the mermaid node IDs."
	user := fmt.Sprintf("Project: %q\nDescription: %s%s", name, desc, ctxNote)

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
			Model:       model,
			Messages:    []core.Message{{Role: "system", Content: sys}, {Role: "user", Content: user}},
			Temperature: &temp,
		})
		if err != nil || len(resp.Choices) == 0 {
			log.Printf("[server] plan/workflow call failed (attempt %d): %v", attempt, err)
			if err != nil {
				genErr = err
			}
			// A non-retryable failure (e.g. daily-quota exhaustion) won't
			// improve on a second attempt — stop immediately so we surface the
			// real reason fast instead of trying twice.
			if err != nil && !llm.IsRetryable(err) {
				break
			}
			continue
		}
		plan, workflow = splitPlanWorkflow(strings.TrimSpace(resp.Choices[0].Message.Content))
		if plan != "" || workflow != "" {
			return plan, workflow, nil
		}
	}
	return "", "", genErr
}

// splitPlanWorkflow tolerantly splits a combined plan+workflow response into
// its two parts, preferring the explicit ===WORKFLOW=== delimiter and falling
// back to the first checkbox section when the model omits it.
func splitPlanWorkflow(text string) (plan, workflow string) {
	for _, delim := range []string{"===WORKFLOW===", "=== WORKFLOW ===", "==WORKFLOW=="} {
		if i := strings.Index(text, delim); i >= 0 {
			return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+len(delim):])
		}
	}
	// No delimiter: split at the section heading that precedes the first
	// workflow checkbox, if any; otherwise treat the whole thing as the plan.
	if idx := strings.Index(text, "- [ ]"); idx >= 0 {
		if head := strings.LastIndex(text[:idx], "\n## "); head >= 0 {
			return strings.TrimSpace(text[:head]), strings.TrimSpace(text[head:])
		}
		return strings.TrimSpace(text[:idx]), strings.TrimSpace(text[idx:])
	}
	return strings.TrimSpace(text), ""
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

// needsPlanAmend reports whether query should trigger a synchronous plan
// rewrite before Execute runs. It skips the amend only for a bare continuation
// ("continue"/"yes") after a real prior turn, using the same signal as the
// clarification gate; anything else is a plausible new instruction.
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
// instruction before the kernel executes it, so execution runs against a fresh
// plan. Bounded by a tight sub-timeout; on timeout/error the old plan/workflow
// are returned unchanged (fail-open).
func (s *Server) amendPlanWorkflowSync(ctx context.Context, projID, query, oldPlan, oldWorkflow string) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Prefer the local model for these two rewrites when one is healthy
	// (Part 5b) — else the cloud primary, unchanged. RouteAux returns a
	// core.LLMClient; fall back to primaryClient when no model routes.
	client, clientModel := s.primaryClientModel()
	if s.kernel != nil {
		if lc, lm, ok := s.kernel.RouteAux("plan_amend", 0); ok && lc != nil {
			client, clientModel = lc, lm
		}
	}
	temp := 0.2

	plan, workflow := oldPlan, oldWorkflow
	sys := "You are an AI software architect. Rewrite a project's plan and workflow to reflect a new instruction, BEFORE any work is done. Output ONLY raw markdown in TWO sections separated by a line containing exactly ===WORKFLOW===\n" +
		"Section 1 = the updated Implementation Plan: keep the ```mermaid\\ngraph TD``` architecture; node IDs T1,T2,... MUST match workflow task IDs; reuse existing IDs, add new ones only for new tasks; add an 'Open Questions' section if underspecified.\n" +
		"Section 2 = the updated Task Workflow: \"- [ ] T<n>: ...\" (pending) / \"- [/] T<n>: ...\" (running — mark the task worked on next) / \"- [x] T<n>: ...\" (done); keep task IDs stable, never renumber."
	user := fmt.Sprintf("Current Implementation Plan:\n%s\n\nCurrent Task Workflow:\n%s\n\nThe user just requested: %s", oldPlan, oldWorkflow, query)
	if resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
		Model:       clientModel,
		Messages:    []core.Message{{Role: "system", Content: sys}, {Role: "user", Content: user}},
		Temperature: &temp,
	}); err == nil && len(resp.Choices) > 0 {
		np, nw := splitPlanWorkflow(strings.TrimSpace(resp.Choices[0].Message.Content))
		if np != "" {
			plan = np
		}
		if nw != "" {
			workflow = nw
		}
	}

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

// injectNodeStatus styles the plan's mermaid fence so the architecture graph
// reflects the workflow's task status (green=done, amber=running, gray=pending).
// No-op if there's no mermaid fence or no ID'd task lines to map.
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
	mux.Handle("/api/chat", s.csrfMiddleware(s.idempotencyMiddleware(http.HandlerFunc(s.handleChat))))
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

	// OpenAI-compatible surface: any client that speaks that wire format can
	// point its base_url here and use DarkCode as the model.
	mux.HandleFunc("/v1/models", s.handleOpenAIModels)
	mux.HandleFunc("/v1/chat/completions", s.handleOpenAIChat)

	// Architecture extensions
	mux.HandleFunc("/api/checkpoints", s.handleCheckpoints)
	mux.HandleFunc("/api/checkpoints/diff", s.handleCheckpointDiff)
	mux.HandleFunc("/api/rollback", s.handleRollback)
	mux.HandleFunc("/api/runs", s.handleRunEvents)
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

// securityHeaders adds defense-in-depth browser security headers (nosniff,
// frame-deny, no-referrer) to every response. Cheap even though the server is
// loopback-only, in case the UI is ever proxied or framed.
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

// SetGUIActive arms (GUI) or disarms (CLI) disconnect-driven CLI resume.
// main.go calls it on every CLI↔GUI transition. Disarming in CLI mode stops a
// leftover browser tab's SSE disconnect from firing the grace timer and
// corrupting the terminal prompt.
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
