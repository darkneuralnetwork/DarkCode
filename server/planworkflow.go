package server

// planworkflow.go — keeping a project's blueprint in step with the work.
//
// A project carries two long-lived documents: an implementation plan and a task
// workflow. They are seeded from a skeleton, rewritten by a model from the
// project description, amended when a new instruction arrives, and stamped with
// live task status for the Blueprint tab.
//
// None of that is HTTP. It sat in server.go only because that is where the
// handlers calling it lived.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/llm"
	"github.com/darkcode/orchestrator"
	"github.com/darkcode/planwork"
)

// summaryThreshold is the minimum raw context size (bytes) before the server
// bothers generating a compressed summary. Below this the raw context is small
// enough to inject verbatim.
const summaryThreshold = 12 * 1024 // 12 KiB

// summaryRegrowth is the minimum growth in raw context size (bytes) since the
// last summary generation before the server recompresses. This prevents
// recompressing on every single exchange (cost/latency) while keeping the
// summary reasonably fresh.
const summaryRegrowth = 8 * 1024 // 8 KiB

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
				planText = planwork.InjectNodeStatus(planText, wfText)
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
		plan, workflow = planwork.Split(strings.TrimSpace(resp.Choices[0].Message.Content))
		if plan != "" || workflow != "" {
			return plan, workflow, nil
		}
	}
	return "", "", genErr
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
	// One implementation, shared with the console. This used to be a second
	// copy of the same prompt; see planwork's package comment for what the two
	// copies disagreed about.
	plan, workflow := planwork.Amend(ctx, client, clientModel, query, oldPlan, oldWorkflow)

	plan = planwork.InjectNodeStatus(plan, workflow)

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
