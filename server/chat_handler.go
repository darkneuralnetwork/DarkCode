package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/darkcode/attach"
	"github.com/darkcode/core"
	"github.com/darkcode/metrics"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/project"
	"github.com/darkcode/router"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Query       string              `json:"query"`
		Mode        string              `json:"mode"`        // single, escalation, consensus
		ChatMode    string              `json:"chat_mode"`   // general, project, auto
		Safety      string              `json:"safety"`      // strict, normal, relaxed
		Brain       string              `json:"brain"`       // local, cloud, auto (per-request routing preference)
		Project     string              `json:"project"`     // optional active project id
		Attachments []attach.Attachment `json:"attachments"` // optional file/dir/image/url/text refs
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}

	// Count this as one user turn (question). The per-request LLM-call counter
	// is separate — one turn fans out into several calls — so the telemetry can
	// show both and their ratio rather than conflating them.
	metrics.Default.RecordTurn()

	s.cfgMu.RLock()
	primaryModel := s.cfg.Model
	enableLocal := s.cfg.EnableLocalLLM
	s.cfgMu.RUnlock()

	if primaryModel == "" && !enableLocal {
		writeError(w, http.StatusBadRequest, "please select a model or initialise the local llm")
		return
	}

	// Per-request routing-mode / safety-level / loop overrides. These are
	// applied to the LIVE router, permission gate, and loop flag (what
	// Execute actually uses) for the duration of this request only, then
	// restored. We deliberately do NOT mutate s.cfg here: the override is
	// per-request, so /api/status and /api/config keep reflecting the
	// configured (startup) state.
	//
	// Loop engineering is an explicit chat mode: the ReAct loop runs only
	// when the user picks "Loop" mode AND the master toggle is ON in Settings
	// (the dropdown hides the option otherwise, but we re-check the master
	// here as defense-in-depth). General/Project/Auto explicitly force the
	// loop OFF so a globally-enabled loop never silently takes over those
	// modes — the loop is opt-in per request.
	// Smart Auto-Detection Mode (Advanced Intent Classification)
	s.cfgMu.RLock()
	masterLoop := s.cfg.AgenticLoop
	s.cfgMu.RUnlock()

	if req.ChatMode == "smart" || req.ChatMode == "auto" || req.ChatMode == "" {
		// Cost guard: for a query that is obviously a general question, skip the
		// LLM intent-classifier call (and the project auto-creation it can
		// trigger) entirely — route straight to lean General mode. This keeps a
		// plain question at a single model call instead of classifier + answer
		// (+ possible project plan/workflow generation). Ambiguous or clearly
		// tool-worthy queries still fall through to the LLM classifier below.
		if router.IsGeneralQuestion(req.Query) {
			req.ChatMode = "general"
		}
	}

	if req.ChatMode == "smart" || req.ChatMode == "auto" || req.ChatMode == "" {
		client := s.primaryClient()
		prompt := fmt.Sprintf(`Analyze this user query: %q.
Determine the required execution mode:
- "general": A simple question, explanation, or chat that does NOT require using any tools or modifying files.
- "project": A task that requires using tools (reading/writing files, searching) but is relatively straightforward.
- "loop": A complex, multi-step task (like building an app, a massive refactor, or complex debugging) that requires a continuous agentic loop.
Output ONLY JSON: {"mode": "general|project|loop", "is_new_project": true/false, "project_name": "...", "project_description": "..."}`, req.Query)

		temp := 0.0
		llmReq := &core.CompletionRequest{
			Messages: []core.Message{
				{Role: "system", Content: "You are an orchestration classifier. Output only valid JSON."},
				{Role: "user", Content: prompt},
			},
			Temperature: &temp,
		}

		// If local LLM is enabled and a classifier LoRA exists, we could mount it here.
		// For now, we rely on the primary model's intelligence.
		resp, err := client.ChatCompletion(r.Context(), llmReq)
		if err == nil && len(resp.Choices) > 0 {
			text := resp.Choices[0].Message.Content
			var result struct {
				Mode         string `json:"mode"`
				IsNewProject bool   `json:"is_new_project"`
				ProjectName  string `json:"project_name"`
				ProjectDesc  string `json:"project_description"`
			}
			if err := json.Unmarshal([]byte(text), &result); err == nil {
				req.ChatMode = result.Mode // switch to the detected mode

				if result.IsNewProject && req.Project == "" && s.projects != nil {
					if proj, err := s.projects.Create(result.ProjectName, result.ProjectDesc, s.ActiveWorkspace(), nil); err == nil {
						req.Project = proj.ID
						if s.emitter != nil {
							s.emitter.EmitTaskUpdate("project_auto_created", proj.ID, proj.Name)
						}
						s.seedProjectPlanWorkflow(proj.ID, proj.Name, proj.Description, "")
					}
				}
			}
		}
	}

	// Re-evaluate overrides after Smart Mode classification
	loopOverride := "off"
	if req.ChatMode == "loop" && masterLoop {
		loopOverride = "on"
	}
	toolsOverride := "on"
	if req.ChatMode == "general" {
		toolsOverride = "off"
	}
	restoreOverrides := s.kernel.ApplyRequestOverrides(req.Mode, req.Safety, loopOverride, toolsOverride, req.Brain)
	defer restoreOverrides()

	// If an active project is specified, prepend its long-lived context to
	// the query so the agent operates with project knowledge in scope.
	//
	// Compression-aware injection: when a compressed summary exists we inject
	// the compact summary (+ a short recent-activity tail) INSTEAD of the raw
	// context.md. This keeps the prompt small even after a project has
	// accumulated a large context.md across many sessions — the LLM still gets
	// a faithful advance briefing (summary) plus the freshest exchanges (tail).
	// When no summary has been generated yet (small/new project) the raw
	// context is used, preserving the original behavior.
	query := req.Query
	if req.Project != "" && s.projects != nil {
		query = s.projects.BuildContextQuery(req.Project, req.Query)
	}

	// Resolve any attachments (file/dir/image/url/text) into a markdown block
	// prepended to the query so the agent has the material in scope. Relative
	// paths resolve against the active workspace.
	if len(req.Attachments) > 0 {
		block, results := attach.Resolve(req.Attachments, s.ActiveWorkspace())
		query = block + query
		// Surface attachment resolution status via the event stream so the GUI
		// can show which attachments loaded.
		if s.emitter != nil {
			for _, r := range results {
				status := "attached"
				if !r.OK {
					status = "attachment error"
				}
				s.emitter.EmitTaskUpdate("attachments", status, r.Type+" "+r.Source)
			}
		}
	}

	if s.emitter != nil {
		s.emitter.EmitChatQuery(req.Query)
	}

	// Run the orchestrator kernel
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	s.activeChatCancelMu.Lock()
	s.activeChatCancel = cancel
	s.activeChatCancelMu.Unlock()
	defer func() {
		s.activeChatCancelMu.Lock()
		s.activeChatCancel = nil
		s.activeChatCancelMu.Unlock()
	}()

	// Inject the workspace into context
	ws := s.ActiveWorkspace()
	if req.Project != "" && s.projects != nil {
		if p, err := s.projects.Get(req.Project); err == nil {
			ws = p.Path
		}
	}
	ctx = context.WithValue(ctx, core.WorkspaceKey, ws)
	ctx = context.WithValue(ctx, core.ProjectKey, req.Project)

	// Inject the active project's implementation plan + workflow architecture
	// so the kernel's planner follows the plan. The plan/workflow are amended
	// SYNCHRONOUSLY here, before Execute runs, when the incoming message
	// looks like a new instruction (see needsPlanAmend) — "if a new
	// instruction comes, first change these, then go through these only"
	// (local-first upgrade §5). This replaces the previous design where the
	// rewrite ran in a goroutine launched only AFTER Execute returned
	// (racing with a separate pre-Execute "mark task running" goroutine), so
	// execution used to always run against a stale plan. Cleared after
	// Execute so a subsequent non-project request isn't contaminated.
	// pendingTaskID is the workflow task this turn is about to work on (if
	// resolvable) — captured BEFORE Execute so a successful response can
	// mark it done afterward (local-first upgrade §7 Fix D), closing the
	// loop between execution and the Blueprint tab's live status. Best
	// effort: stays "" when there's no active project or nothing pending,
	// in which case the write-back below is simply skipped.
	var pendingTaskID string
	if req.Project != "" && s.projects != nil {
		plan, _ := s.projects.GetPlan(req.Project)
		workflow, _ := s.projects.GetWorkflow(req.Project)
		s.cfgMu.RLock()
		skipReadOnly := s.cfg.SkipAuxForReadOnly
		s.cfgMu.RUnlock()
		amending := needsPlanAmend(req.Query, s.kernel.RecentSTM(), skipReadOnly)
		if amending {
			plan, workflow = s.amendPlanWorkflowSync(ctx, req.Project, req.Query, plan, workflow)
		}
		if id, _, ok := orchestrator.NextPendingWorkflowTask(workflow); ok {
			pendingTaskID = id
		}
		// Phase 4 — brief-first project memory. On a routine (non-amend) turn the
		// compact, auto-updated project brief is already prepended to the query by
		// BuildContextQuery, so inject only the task workflow (needed for task
		// continuity) instead of the full ~8K implementation plan. The full plan
		// is injected only when the user is actively shaping it (an amend/planning
		// turn). This keeps routine turns small and makes resuming a project cheap
		// — the brief carries "what this project is" without re-feeding the plan.
		if amending {
			s.kernel.SetProjectContext(plan, workflow)
		} else {
			s.kernel.SetProjectContext("", workflow)
		}
		defer s.kernel.ClearProjectContext()
	}

	output, err := s.kernel.Execute(ctx, query)
	if err != nil {
		if s.emitter != nil {
			s.emitter.EmitError(err.Error())
			s.emitter.EmitChatResponse("Error: " + err.Error())
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.emitter != nil {
		s.emitter.EmitChatResponse(output)
	}

	// Write the resolved task's completion back to the workflow (Fix D) —
	// this is what makes Issue 5's status-linked Mermaid graph reflect real
	// progress, and what makes a subsequent "continue" (Fix B) genuinely
	// advance to the NEXT pending task instead of re-resolving the same one.
	// Synchronous (not fire-and-forget): the response the user is about to
	// see should be consistent with the workflow state by the time it's
	// returned. Skipped when nothing was resolved (id == "" — either no
	// active project, or a legacy ID-less workflow line, which
	// MarkTaskStatus can't target) OR when tools were disabled for this
	// turn (General mode is pure conversation — the task genuinely wasn't
	// worked on, so marking it done would be wrong even if Execute
	// "succeeded"). A clarification-only response (needsClarification) can
	// still slip through as a false-positive "done" — Execute doesn't
	// currently distinguish that case from real work in its return value —
	// but that's a narrower, lower-stakes gap than the General-mode one.
	if pendingTaskID != "" && req.Project != "" && s.projects != nil && req.ChatMode != "general" {
		if err := s.projects.MarkTaskStatus(req.Project, pendingTaskID, project.TaskDone); err != nil {
			log.Printf("[server] failed to mark task %s done: %v", pendingTaskID, err)
		} else if updated, err := s.projects.GetWorkflow(req.Project); err == nil && s.emitter != nil {
			s.emitter.EmitWorkflowUpdated(req.Project, updated)
		}
	}

	if req.Project != "" && s.projects != nil {
		// Plan/workflow are already fresh — amended synchronously above,
		// BEFORE Execute ran (see needsPlanAmend/amendPlanWorkflowSync). What
		// remains here is purely passive bookkeeping: append to the raw
		// context backup and let the existing context-window rewriter trim
		// it, neither of which needs to block the chat response. Bound with
		// a timeout + recover so a slow/hung provider or panic can't leak a
		// goroutine or take down the process.
		go func(projID, q, out string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[server] project raw-context append panic: %v", r)
				}
			}()
			if err := s.projects.AppendRawContext(projID, fmt.Sprintf("## User\n%s\n\n## Assistant\n%s\n", q, out)); err != nil {
				log.Printf("[server] failed to append to raw context: %v", err)
			}
			s.maybeRewriteProjectContext(projID)
		}(req.Project, req.Query, output)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"output":  output,
		"success": true,
		"query":   req.Query,
	})
}

func (s *Server) handleCancelChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	s.activeChatCancelMu.Lock()
	if s.activeChatCancel != nil {
		s.activeChatCancel()
		s.activeChatCancel = nil
	}
	s.activeChatCancelMu.Unlock()
	
	if s.approver != nil {
		s.approver.CancelAll()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleTools lists all registered tools.
