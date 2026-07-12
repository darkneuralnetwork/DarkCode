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
	restoreOverrides := s.kernel.ApplyRequestOverrides(req.Mode, req.Safety, loopOverride, toolsOverride)
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
	// so the kernel's planner follows the plan (previously the plan was a
	// write-only display artifact generated after execution). Cleared after
	// Execute so a subsequent non-project request isn't contaminated.
	if req.Project != "" && s.projects != nil {
		plan, _ := s.projects.GetPlan(req.Project)
		workflow, _ := s.projects.GetWorkflow(req.Project)
		s.kernel.SetProjectContext(plan, workflow)
		defer s.kernel.ClearProjectContext()
		
		go func(pID, uQuery string) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			defer func() { recover() }()
			oldWf, _ := s.projects.GetWorkflow(pID)
			if oldWf != "" {
				client := s.primaryClient()
				temp := 0.0
				wfPrompt := fmt.Sprintf("Here is the current Task Workflow:\n%s\n\nThe user just requested: %s\n\nRewrite the workflow to mark the task corresponding to the user's request as 'running' by changing its checkbox from '- [ ]' to '- [/]'. Output ONLY the raw markdown checklist.", oldWf, uQuery)
				wResp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
					Messages: []core.Message{
						{Role: "system", Content: "You are an AI architect. Only output valid markdown checklists. Change the active task checkbox to '- [/]'."},
						{Role: "user", Content: wfPrompt},
					},
					Temperature: &temp,
				})
				if err == nil && len(wResp.Choices) > 0 {
					s.projects.SetWorkflow(pID, wResp.Choices[0].Message.Content)
					if s.emitter != nil {
						s.emitter.EmitWorkflowUpdated(pID, wResp.Choices[0].Message.Content)
					}
				}
			}
		}(req.Project, req.Query)
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

	if req.Project != "" && s.projects != nil {
		go func(projID, q, out string) {
			// Bound the background updater: a timeout prevents goroutine
			// leak / runaway cost when the provider hangs, and a recover
			// keeps a panic from killing the process. Uses the shared
			// primary client builder so telemetry records the provider.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[server] project updater panic: %v", r)
				}
			}()
			client := s.primaryClient()

			temp := 0.0
			// 1. Update Plan
			oldPlan, _ := s.projects.GetPlan(projID)
			planPrompt := fmt.Sprintf("Here is the current Implementation Plan and Architecture:\n%s\n\nUser asked: %s\nAgent did: %s\n\nRewrite the implementation plan to reflect the new state. You MUST keep the Mermaid architecture graph and update it if necessary. If you have any confusion or underspecified requirements, add an 'Open Questions' section to ask the user. Output ONLY the raw markdown plan.", oldPlan, q, out)
			llmReq1 := &core.CompletionRequest{
				Messages: []core.Message{
					{Role: "system", Content: "You are an AI architect. Keep the plan detailed but concise. Only output valid markdown. Always include a Mermaid graph."},
					{Role: "user", Content: planPrompt},
				},
				Temperature: &temp,
			}
			pResp, err := client.ChatCompletion(ctx, llmReq1)
			if err == nil && len(pResp.Choices) > 0 {
				planText := pResp.Choices[0].Message.Content
				s.projects.SetPlan(projID, planText)
				if s.emitter != nil {
					s.emitter.EmitPlanUpdated(projID, planText)
				}
			}

			// 2. Update Workflow
			oldWf, _ := s.projects.GetWorkflow(projID)
			wfPrompt := fmt.Sprintf("Here is the current Task Workflow:\n%s\n\nUser asked: %s\nAgent did: %s\n\nRewrite the task workflow to reflect the new state. Maintain the markdown checklist format (- [ ], - [x]). Output ONLY the raw markdown.", oldWf, q, out)
			llmReq2 := &core.CompletionRequest{
				Messages: []core.Message{
					{Role: "system", Content: "You are an AI architect. Keep the task workflow concise. Only output valid markdown checkboxes."},
					{Role: "user", Content: wfPrompt},
				},
				Temperature: &temp,
			}
			wResp, err := client.ChatCompletion(ctx, llmReq2)
			if err == nil && len(wResp.Choices) > 0 {
				wfText := wResp.Choices[0].Message.Content
				s.projects.SetWorkflow(projID, wfText)
				if s.emitter != nil {
					s.emitter.EmitWorkflowUpdated(projID, wfText)
				}
			}

			// 3. Append to raw backup and automatically rewrite the project context.
			// The user specified that context should be rewritten after every user message response,
			// and to keep a hidden raw backup without using it in conversation.
			if err := s.projects.AppendRawContext(projID, fmt.Sprintf("## User\n%s\n\n## Assistant\n%s\n", q, out)); err != nil {
				log.Printf("[server] failed to append to raw context: %v", err)
			}

			// Trigger a rewrite using the dedicated rewrite logic
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
