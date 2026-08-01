package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/darkcode/attach"
	"github.com/darkcode/core"
	"github.com/darkcode/metrics"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/plan"
	"github.com/darkcode/project"
	"github.com/darkcode/router"
	"github.com/darkcode/verb"
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

	// A leading strategy verb is stripped before anything reads the query, so
	// the classifier below sees the task rather than the instruction about how
	// to run it. Without this the web UI sent "/loop fix the parser" to the
	// model as literal text while the console understood it — one intent with
	// two spellings, which is what package verb exists to prevent.
	strippedQuery, verbStrategy, verbFound := stripStrategyVerb(req.Query)
	req.Query = strippedQuery

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
	enableLocal := s.cfg.LocalEnabled()
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
	// Strategy is chosen by progressive escalation rather than by asking a
	// model to predict it. The classifier that used to sit here spent a call
	// with a 12-second timeout guessing how hard the request would be, was the
	// first thing a metered tier rate-limited, and fell back to a keyword scan
	// when it failed — so the keyword scan was carrying it in exactly the
	// conditions that mattered. Now that scan IS the entry point, and the run
	// climbs from there on evidence.
	effort, why := router.EntryEffort(req.Query)
	if verbFound {
		// An explicit verb is a decision already made, so there is nothing to
		// infer and nothing to announce.
		req.ChatMode = chatModeForVerb(verbStrategy)
	} else {
		req.ChatMode = chatModeForEffort(effort)
		s.announceEffort(effort, why)
	}

	// Project auto-creation stays deterministic. It was already independent of
	// the classifier's opinion — the is_new_project field proved too flaky to
	// carry the feature — so it reads the same signal the routing does.
	if req.Project == "" && router.IsBuildShaped(req.Query, effort) && !verbFound {
		if id := s.autoCreateProject(req.Query, "", ""); id != "" {
			req.Project = id
		}
	}

	// Re-evaluate overrides after Smart Mode classification
	// The requested mode alone decides this. It used to also require a
	// persistent master toggle, so choosing Loop could silently do nothing.
	loopOverride := "off"
	if req.ChatMode == "loop" {
		loopOverride = "on"
	}
	toolsOverride := "on"
	if req.ChatMode == "general" {
		// Chat mode: read-only tools (read/list/search/web), never writes.
		toolsOverride = "readonly"
	}
	if verbFound {
		if verbStrategy.Loop != "" {
			loopOverride = verbStrategy.Loop
		}
		if verbStrategy.Tools != "" {
			toolsOverride = verbStrategy.Tools
		}
		if verbStrategy.Mode != "" {
			req.Mode = verbStrategy.Mode
		}
	}
	restoreOverrides := s.kernel.ApplyRequestOverrides(req.Mode, req.Safety, loopOverride, toolsOverride, req.Brain)
	defer restoreOverrides()
	if verbFound {
		restorePlan := s.kernel.ApplyPlanOverride(verbStrategy.Plan)
		defer restorePlan()
		if verbStrategy.Debate {
			restoreDebate := s.kernel.ApplyDebateOverride(true)
			defer restoreDebate()
		}
	}

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

	// Run the orchestrator kernel under a deadline that measures SILENCE
	// rather than total elapsed time — see progress_deadline.go. The flat
	// five-minute cap this replaces covered Execute plus both completeness
	// auto-continue passes, so a build that was steadily making progress got
	// cancelled mid-step once the turn crossed five minutes.
	ctx, cancel := s.progressContext(context.Background(), chatIdleTimeout, chatHardTimeout)
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
	var pendingTaskID, pendingTaskLine string
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
		if id, line, ok := orchestrator.NextPendingWorkflowTask(workflow); ok {
			pendingTaskID = id
			pendingTaskLine = line
		}
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
	// Subtask verification + completeness auto-continue (ChatManager). Build
	// executes one workflow subtask per turn; before ticking it [x] we VERIFY
	// its implied deliverable actually exists — a subtask that says "create
	// index.html" isn't done until an .html file is present. Then, if the whole
	// request's expected artifacts are still incomplete, auto-continue once to
	// finish (bounded), so "make a website" can't stop having skipped the .js.
	cm := orchestrator.NewChatManager()
	// A plan-proposal turn produced no artifacts — the output is the plan
	// preview awaiting the user's approve/revise/reject. Running the build
	// completeness check against it would find "gaps" and auto-continue,
	// re-entering the kernel and mangling the pending plan, so both the
	// completeness pass and the subtask tick are skipped on proposal turns.
	awaitingPlan := s.kernel != nil && s.kernel.PlanAwaitingApproval()
	buildTurn := req.ChatMode != "general" && !awaitingPlan // Chat is read-only; it never "builds"
	if buildTurn {
		output = s.completeBuild(ctx, cm, req.Query, ws, output)
	}
	if pendingTaskID != "" && req.Project != "" && s.projects != nil && buildTurn {
		// Only mark the subtask done when its own deliverable is verified present.
		taskDone, taskGaps := cm.CheckCompleteness(pendingTaskLine, ws)
		if !taskDone {
			log.Printf("[server] task %s left pending — unmet: %v", pendingTaskID, taskGaps)
		} else if err := s.projects.MarkTaskStatus(req.Project, pendingTaskID, project.TaskDone); err != nil {
			log.Printf("[server] failed to mark task %s done: %v", pendingTaskID, err)
		} else if updated, err := s.projects.GetWorkflow(req.Project); err == nil && s.emitter != nil {
			s.emitter.EmitWorkflowUpdated(req.Project, updated)
		}
	}

	// Persist the plan graph to the active project. An EXECUTED graph (with
	// final node statuses) is saved as graph.json — the typed source of
	// truth — and, when the project's blueprint is still the seed skeleton
	// (or empty), the graph is also rendered into plan.md/workflow.md so the
	// Blueprint tab shows the real plan instead of "Awaiting plan
	// generation". A hand-shaped blueprint is never clobbered. A PENDING
	// proposal gets the same render treatment so the user can review the
	// plan in the Blueprint tab while it awaits approval in chat.
	if s.kernel != nil && req.Project != "" && s.projects != nil {
		if g, ok := s.kernel.ConsumeApprovedPlan(); ok {
			if b := g.Bytes(); b != nil {
				if err := s.projects.SetPlanGraph(req.Project, b); err != nil {
					log.Printf("[server] failed to persist plan graph: %v", err)
				}
			}
			s.upgradeBlueprintFromGraph(req.Project, g)
		} else if g, ok := s.kernel.PendingPlanGraph(); ok && awaitingPlan {
			s.upgradeBlueprintFromGraph(req.Project, g)
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

// maxCompletePasses bounds the ChatManager's auto-continue so a stuck build
// can't loop forever; cost is incurred only when a real gap is detected.
const maxCompletePasses = 2

// completeBuild runs bounded completeness auto-continue: while the goal's
// expected artifacts are missing from the workspace, it re-invokes the kernel
// with a focused corrective goal so a Build finishes what it started instead of
// stopping with skipped deliverables (the "made a website but no .js" failure).
// Appends each corrective result to the output. No-op when nothing is missing.
func (s *Server) completeBuild(ctx context.Context, cm *orchestrator.ChatManager, goal, workspace, output string) string {
	for pass := 0; pass < maxCompletePasses; pass++ {
		done, gaps := cm.CheckCompleteness(goal, workspace)
		if done {
			break
		}
		if s.emitter != nil {
			s.emitter.EmitTaskUpdate("complete", "auto-continue", "Incomplete — creating: "+strings.Join(gaps, ", "))
		}
		corrective := fmt.Sprintf("The work so far is INCOMPLETE for the goal %q. It is still missing: %s. Create ONLY the missing file(s) now, with real, working content — do not repeat what already exists.",
			goal, strings.Join(gaps, ", "))
		more, err := s.kernel.Execute(ctx, corrective)
		if err != nil {
			log.Printf("[server] completeness auto-continue failed: %v", err)
			break
		}
		output += "\n\n" + strings.TrimSpace(more)
	}
	return output
}

// autoCreateProject creates a project for a build task, seeds its blueprint
// skeleton, and returns the new project id (or "" on failure). Name/desc fall
// back to values derived from the query when the classifier didn't supply
// them.
func (s *Server) autoCreateProject(query, name, desc string) string {
	if s.projects == nil {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = deriveProjectName(query)
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		desc = query
	}
	proj, err := s.projects.Create(name, desc, s.ActiveWorkspace(), nil)
	if err != nil {
		log.Printf("[server] project auto-creation failed: %v", err)
		return ""
	}
	log.Printf("[server] auto-created project %s (%q) for build task", proj.ID, proj.Name)
	if s.emitter != nil {
		s.emitter.EmitTaskUpdate("project_auto_created", proj.ID, proj.Name)
	}
	s.seedProjectPlanWorkflow(proj.ID, proj.Name, proj.Description, "")
	return proj.ID
}

// deriveProjectName builds a short human project name from the query when
// the classifier didn't supply one: the first few meaningful words, minus
// leading creation verbs and articles.
func deriveProjectName(query string) string {
	words := strings.Fields(strings.TrimSpace(query))
	skip := map[string]bool{
		"create": true, "build": true, "make": true, "implement": true,
		"develop": true, "write": true, "generate": true, "scaffold": true,
		"please": true, "a": true, "an": true, "the": true, "me": true,
		"new": true, "set": true, "up": true, "setup": true,
	}
	var kept []string
	for _, w := range words {
		lw := strings.ToLower(strings.Trim(w, ".,!?:;\"'"))
		if len(kept) == 0 && skip[lw] {
			continue
		}
		if lw == "" {
			continue
		}
		kept = append(kept, strings.Trim(w, ".,!?:;\"'"))
		if len(kept) >= 5 {
			break
		}
	}
	name := strings.Join(kept, " ")
	if len(name) > 48 {
		name = name[:48]
	}
	if strings.TrimSpace(name) == "" {
		name = "Untitled Build"
	}
	return name
}

// upgradeBlueprintFromGraph renders a plan graph into the project's
// plan.md/workflow.md — but only over an empty or still-skeleton blueprint,
// so a hand-shaped project plan is never clobbered by a per-request graph.
func (s *Server) upgradeBlueprintFromGraph(projID string, g *plan.Graph) {
	if cur, _ := s.projects.GetPlan(projID); isSkeletonBlueprint(cur) {
		planMD := plan.RenderMarkdown(g)
		if err := s.projects.SetPlan(projID, planMD); err == nil && s.emitter != nil {
			s.emitter.EmitPlanUpdated(projID, planMD)
		}
	}
	if cur, _ := s.projects.GetWorkflow(projID); isSkeletonBlueprint(cur) {
		wf := plan.RenderWorkflow(g)
		if err := s.projects.SetWorkflow(projID, wf); err == nil && s.emitter != nil {
			s.emitter.EmitWorkflowUpdated(projID, wf)
		}
	}
}

// isSkeletonBlueprint reports whether a stored plan/workflow is still the
// idempotent seed skeleton (or empty) — i.e. safe to overwrite with a real
// rendered plan.
func isSkeletonBlueprint(content string) bool {
	c := strings.TrimSpace(content)
	return c == "" || strings.Contains(c, "_Status: awaiting first task_")
}

func (s *Server) handleCancelChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// An optional redirect turns a bare stop into "stop and do this instead".
	// Without it the agent's next turn has no idea why it was interrupted and
	// is liable to resume the abandoned approach.
	var body struct {
		Redirect string `json:"redirect"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.activeChatCancelMu.Lock()
	if s.activeChatCancel != nil {
		s.activeChatCancel()
		s.activeChatCancel = nil
	}
	s.activeChatCancelMu.Unlock()

	if s.approver != nil {
		s.approver.CancelAll()
	}

	if redirect := strings.TrimSpace(body.Redirect); redirect != "" && s.memSystem != nil {
		s.memSystem.STMAdd(core.Message{
			Role:    core.RoleUser,
			Content: "[Interrupted the previous task] Stop that approach. New instruction: " + redirect,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleTools lists all registered tools.

// chatModeForVerb maps a strategy onto the chat mode the rest of the handler
// reads. The verb is the authority: a read-only verb must not be routed down a
// path that treats the turn as a build.
func chatModeForVerb(st verb.Strategy) string {
	switch {
	case st.Loop == "on":
		return "loop"
	case st.Tools == "readonly":
		return "general"
	default:
		return "project"
	}
}

// stripStrategyVerb removes a leading strategy verb from a query and returns
// the strategy it selected. A query with no verb comes back unchanged.
func stripStrategyVerb(query string) (string, verb.Strategy, bool) {
	if st, rest, ok := verb.Split(query); ok {
		return rest, st, true
	}
	return query, verb.Strategy{}, false
}

// chatModeForEffort maps an escalation rung onto the chat mode the rest of the
// handler reads.
func chatModeForEffort(e router.Effort) string {
	switch e {
	case router.EffortAsk:
		return "general"
	case router.EffortLoop, router.EffortGraph:
		return "loop"
	default:
		return "project"
	}
}

// announceEffort tells the user which strategy was chosen and why.
//
// Not decoration. A silent strategy change is indistinguishable from a bug when
// the cost or latency jumps, and it is how the verbs get discovered: watching
// the tool reach for /graph is what teaches someone to type it themselves next
// time, for a task whose shape they already know.
//
// It stays quiet for the default rung. Announcing every ordinary message would
// make this the most frequent event in the feed and the first one people learn
// to skip — and there is no verb to teach for the rung you get by typing
// nothing.
func (s *Server) announceEffort(e router.Effort, why string) {
	v := e.Verb()
	if s.emitter == nil || v == "" {
		return
	}
	s.emitter.EmitTaskUpdate("strategy", string(e), why+" — same as /"+v)
}
