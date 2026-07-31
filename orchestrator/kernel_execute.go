package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/darkcode/compression"
	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/loop"
	"github.com/darkcode/plan"
	"github.com/darkcode/router"
)

// ============================================================================
// EXECUTION LOOP: Observe → Compress → Plan → Route → Execute → Validate → Merge → Store
// ============================================================================

// Execute is the main entry point. It runs the full execution loop.
// maxPlanInjectBytes caps how much of the plan/workflow markdown is
// injected into the goal, so a very long plan can't blow out the context
// window. The plan is truncated with a marker when it exceeds this.
const maxPlanInjectBytes = 8192

// SetProjectContext stashes the active project's implementation plan and
// workflow architecture so Execute can inject them into the goal for the
// current request. Call ClearProjectContext (usually via defer) afterwards.
// Safe to call with empty strings (no-op injection). The kernel deliberately
// takes raw strings (not a *project.Store) to avoid coupling the orchestrator
// to the project package.
func (k *Kernel) SetProjectContext(plan, workflow string) {
	k.mu.Lock()
	k.projectPlan = plan
	k.projectWorkflow = workflow
	k.mu.Unlock()
}

// ClearProjectContext resets the per-request plan/workflow stash. Call after
// Execute (typically deferred) so a subsequent non-project request isn't
// contaminated with the previous project's plan.
func (k *Kernel) ClearProjectContext() {
	k.mu.Lock()
	k.projectPlan = ""
	k.projectWorkflow = ""
	k.mu.Unlock()
}

// RecentSTM returns the current short-term-memory conversation window. Used
// by the server layer (which doesn't hold k.memory directly, by design —
// the kernel is decoupled from project.Store/persistence concerns) to judge
// whether an incoming message is a bare continuation of an active
// conversation — see HasActiveConversation. Read-only; safe to call at any
// point in the request lifecycle.
func (k *Kernel) RecentSTM() []core.Message {
	return k.memory.STMGet()
}

// HasActiveConversation reports whether stm holds a real ongoing conversation
// (at least one prior assistant turn) rather than a cold start. The current
// turn is assumed already appended, so index -2 is the prior turn. Shared by
// the clarification and blueprint-amend gates so both agree on what counts as
// a continuation.
func HasActiveConversation(stm []core.Message) bool {
	return len(stm) >= 2 && stm[len(stm)-2].Role == core.RoleAssistant
}

// injectProjectContext prepends the stashed plan/workflow (if any) to the
// goal so every execution path (general, loop, trivial-direct, DAG) follows
// the active project's plan. No-op when no project is active.
func (k *Kernel) injectProjectContext(goal string) string {
	k.mu.Lock()
	plan := k.projectPlan
	workflow := k.projectWorkflow
	k.mu.Unlock()
	plan = strings.TrimSpace(plan)
	workflow = strings.TrimSpace(workflow)
	if plan == "" && workflow == "" {
		return goal
	}
	var sb strings.Builder
	sb.WriteString("IMPORTANT EXECUTION DIRECTIVE:\nAll your implementations, tool calls, and responses MUST strictly adhere to the provided Implementation Plan, Architecture, and Task Workflow below. You are not allowed to deviate from these documents.\n\n")
	if plan != "" {
		sb.WriteString("## Implementation Plan & Architecture\n")
		sb.WriteString(strutil.TruncateMid(plan, maxPlanInjectBytes))
		sb.WriteString("\n\n")
	}
	if workflow != "" {
		sb.WriteString("## Task Workflow\n")
		sb.WriteString(strutil.TruncateMid(workflow, maxPlanInjectBytes))
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Task Goal\n")
	if enriched, ok := resolveTaskGoal(goal, workflow); ok {
		sb.WriteString(enriched)
	} else {
		sb.WriteString(goal)
	}
	return sb.String()
}

// shortContinuationMaxLen bounds what counts as a "bare continuation"
// ("continue", "yes", "go on") for resolveTaskGoal — mirrors the length
// heuristic server.needsPlanAmend uses for the identical judgment call on
// the blueprint-amend side. The two packages can't share the constant
// directly (server depends on orchestrator, not the reverse) but
// intentionally agree on scale.
const shortContinuationMaxLen = 30

// resolveTaskGoal enriches a bare continuation goal ("continue") with the
// concrete next-pending workflow task. Returns (enriched, true) only when the
// goal is a continuation AND a pending task was found; otherwise ("", false)
// and the caller uses the raw goal.
func resolveTaskGoal(goal, workflow string) (string, bool) {
	if len(strings.TrimSpace(goal)) >= shortContinuationMaxLen {
		return "", false
	}
	id, line, ok := NextPendingWorkflowTask(workflow)
	if !ok {
		return "", false
	}
	if id != "" {
		return fmt.Sprintf("Continue with the next pending workflow task: %s — %s. (User said: %q)", id, line, goal), true
	}
	return fmt.Sprintf("Continue with the next pending workflow task: %s. (User said: %q)", line, goal), true
}

// workflowTaskLineRe / workflowLegacyTaskLineRe parse a workflow checklist's
// pending-task lines. The primary pattern matches the "- [ ] T<n>: <desc>"
// format written by project.Store.MarkTaskStatus / stamped by
// server.injectNodeStatus; the legacy fallback matches a bare "- [ ] <desc>"
// line with no task ID (workflows seeded before the T<n> convention), so
// they still resolve to *something* actionable rather than nothing.
var (
	workflowTaskLineRe       = regexp.MustCompile(`^\s*-\s*\[ \]\s*([A-Za-z0-9_-]+):\s*(.*)$`)
	workflowLegacyTaskLineRe = regexp.MustCompile(`^\s*-\s*\[ \]\s*(.+)$`)
)

// NextPendingWorkflowTask returns the first pending ("- [ ]") task line in
// workflow markdown: its ID (empty for a legacy ID-less line) and description,
// or ("", "", false) if none. Exported so the server can resolve the same task
// ID to mark done after a successful loop execution.
func NextPendingWorkflowTask(workflow string) (id, line string, ok bool) {
	for _, l := range strings.Split(workflow, "\n") {
		if m := workflowTaskLineRe.FindStringSubmatch(l); m != nil {
			return m[1], strings.TrimSpace(m[2]), true
		}
	}
	for _, l := range strings.Split(workflow, "\n") {
		if m := workflowLegacyTaskLineRe.FindStringSubmatch(l); m != nil {
			return "", strings.TrimSpace(m[1]), true
		}
	}
	return "", "", false
}

func (k *Kernel) Execute(ctx context.Context, userGoal string) (string, error) {
	k.log("observe", "Processing user goal: "+userGoal)

	// Step 0: Cost governor — enforce optional spend caps before doing any
	// expensive work. No-op unless a budget is configured (governor == nil or
	// no cap set). In "block" mode a reached cap refuses the request up front;
	// in "warn" mode it logs and proceeds.
	k.mu.Lock()
	gov := k.governor
	k.mu.Unlock()
	if gov != nil {
		if d := gov.Check(); d.Reason != "" {
			if !d.Allowed {
				k.log("budget", "Request blocked: "+d.Reason)
				return "", fmt.Errorf("cost limit reached: %s (raise the limit or switch to a local model to continue)", d.Reason)
			}
			k.log("budget", "Cost limit warning: "+d.Reason)
		}
	}

	// Step 1: Observe — add to STM
	k.memory.STMAdd(core.Message{
		Role:    core.RoleUser,
		Content: userGoal,
	})

	// Step 1.1: Plan approval gate — when a proposed plan is awaiting the
	// user's decision, this turn IS that decision: approve executes the
	// stored graph, reject discards it, anything else is revision feedback.
	// Checked BEFORE the cognition cascade so a cached answer can never
	// swallow an "approve", and only in tool-enabled modes (General/Chat
	// requests pass through untouched — the pending plan stays pending).
	if !k.toolsDisabledForRequest() && !k.readOnlyForRequest() {
		if out, handled, err := k.handlePendingPlan(ctx, userGoal); handled {
			return out, err
		}
	}

	// Step 1.2: Cognition cascade — try the cost-ascending retrieval rungs
	// (deterministic tools → cache → knowledge graph) before any LLM work. A
	// confident hit answers with zero LLM calls; anything else escalates. Runs
	// on the RAW goal so the rungs match what the user asked, not the
	// plan-injected version. See cascade.go.
	if answer, ok := k.runCascade(ctx, userGoal); ok {
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: answer})
		if k.emitter != nil {
			k.emitter.EmitFinalOutput(answer)
		}
		return answer, nil
	}

	userGoal = k.injectProjectContext(userGoal)

	// Step 2: Compress context (if enabled and history is long enough to be
	// worth an LLM call). Running compression on every request — including a
	// trivial single-turn "what is 2+2" — adds latency and cost for no benefit,
	// so we skip it until the STM window has accumulated meaningful history.
	if k.cfg.CompressContext && k.compressor != nil {
		stm := k.memory.STMGet()
		// Compress when EITHER there's enough message-count growth (the
		// original heuristic) OR the STM already exceeds ~60% of the primary
		// client's window (Part 3 token-budget trigger). The token trigger
		// catches a single giant turn that would overflow a small (local)
		// window even at message #2 — the message-count rule alone never fired
		// for it.
		overTokenBudget := false
		if win := k.primaryContextWindow(); win > 0 {
			if compression.EstimateTokens(stm) > win*60/100 {
				overTokenBudget = true
			}
		}
		countGrown := len(stm) >= compressionMinHistory && len(stm)-k.lastCompressedLen >= compressionMinGrowth
		if countGrown || (overTokenBudget && len(stm) >= 2) {
			k.log("compress", "Compressing context")
			snapshot, err := k.compressor.Compress(ctx, stm, userGoal)
			if err == nil && snapshot != nil {
				k.log("compress", fmt.Sprintf("Context compressed: %d→%d tokens (ratio: %.1f%%)",
					snapshot.OriginalTokens, snapshot.CompressedTokens,
					snapshot.CompressionRatio()))
				if k.emitter != nil {
					k.emitter.EmitCompression(snapshot.OriginalTokens, snapshot.CompressedTokens)
				}
				briefing := compression.SnapshotToMessages(snapshot)
				k.memory.STMCompress(briefing, compressionKeepRecent)
				k.lastCompressedLen = len(k.memory.STMGet())
			}
		}
	}

	// Step 3: Assess complexity
	complexity := router.AssessComplexity(userGoal)
	k.log("plan", fmt.Sprintf("Task complexity: %d/10", complexity))

	// Step 3.01: Fetch Hybrid Recall (Memory + KG) so all paths can use it
	recallBlock := k.getRecallBlock(userGoal)

	// Step 3.02 (MOVED): Confident Recall now runs as rung 1 of the cognition
	// cascade at Step 1.2, before project injection and compression.

	// Step 3.03: Clarification gate — only a cold-start vague action request
	// ("fix it" with no context) with tools enabled gets a clarification, to
	// avoid burning tool turns on a speculative action. See classifyGoalIntent.
	k.mu.Lock()
	hasProjectGuidance := k.projectPlan != "" || k.projectWorkflow != ""
	k.mu.Unlock()
	if !k.toolsDisabledForRequest() &&
		classifyGoalIntent(userGoal, k.memory.STMGet(), hasProjectGuidance) == intentVagueAction {
		k.log("plan", "Request has no actionable subject — requesting clarification")
		clarification := "I can help, but your request doesn't name anything to act on. Tell me what you'd like me to work on — the goal or subject, plus any constraints or examples."
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: clarification})
		k.recordOutcome(userGoal, clarification, nil, false, "clarification", 0, recallBlock)
		if k.emitter != nil {
			k.emitter.EmitFinalOutput(clarification)
		}
		return clarification, nil
	}

	// Step 3.04: Chat (read-only) mode — offer ONLY read-only tools so the
	// model can read/search the project and web to answer, but can never write
	// files or run commands. Output goes to the reply. This is what makes Chat
	// "answer, don't build" while still seeing the project.
	if k.readOnlyForRequest() {
		k.log("plan", "Chat mode (read-only tools) — answer without writing")
		return k.executeChatReadOnly(ctx, userGoal, recallBlock)
	}

	// Step 3.05: General mode fast path — when tool access is disabled for
	// this request (chat_mode=="general"), take a lightweight single-LLM-call
	// path with NO tools offered. This is pure conversation: no DAG, no worker
	// agents, no approval popups, no tool overhead. It is intentionally taken
	// BEFORE the loop/DAG/consensus-trivial branches so General mode never
	// offers tools even if the master loop toggle is on or consensus is set.
	if k.toolsDisabledForRequest() {
		k.log("plan", "General mode (tools disabled) — direct conversational path")
		return k.executeDirectNoTools(ctx, userGoal, recallBlock)
	}

	// Step 3.055: Cost guard — an obvious general question takes the single-call
	// no-tools path instead of the worker+web_search pipeline (several LLM calls
	// for a one-call answer), unless Loop mode or an active project wants tools.
	if !k.loopEnabledForRequest() && !hasProjectGuidance && router.IsGeneralQuestion(userGoal) {
		k.log("plan", "Obvious general question — direct conversational path (no tools)")
		return k.executeDirectNoTools(ctx, userGoal, recallBlock)
	}

	// Step 3.5: Agentic Loop — optional ReAct execution. When enabled, delegate
	// the task to the Sense-Think-Act loop and skip DAG decomposition. If
	// consensus mode is on with multiple models, a synthesis round follows,
	// grounded in the loop's tool trace so reviewers can't claim the agent
	// lacked tool access.
	if k.loopEnabledForRequest() && k.agenticLoop != nil {
		k.log("loop", "Agentic loop (ReAct) enabled — running Sense-Think-Act cycle")
		stm := k.memory.STMGet()
		var history []core.Message
		if len(stm) > 0 {
			history = stm[:len(stm)-1]
		}

		// Plan FIRST, then loop against the plan's acceptance criteria.
		//
		// This used to return before ever reaching the planner below, which is
		// why the two halves of the system never met: the plan graph carried
		// acceptance criteria, expected artifacts and Proof slots, and loop
		// mode — the one mode that can actually iterate toward a target — was
		// the only mode that never saw them. Its stop condition was the model's
		// opinion of its own work. Now the plan supplies the definition of done
		// and running it is what ends the loop.
		//
		// Planning is skipped for goals too small to decompose; those keep the
		// self-evaluation fallback, which is the right check when there is
		// genuinely nothing machine-verifiable to run.
		var contract *loop.Contract
		var planGraph *plan.Graph

		// A criterion the user stated outranks anything the planner would
		// infer. They know when the task is finished; the planner is guessing
		// at it, and a planner call costs a model request to produce a worse
		// answer than the one already in the prompt.
		if criterion, task, ok := loop.ParseUntil(userGoal); ok {
			userGoal = task
			contract = k.untilContract(ctx, criterion)
			k.log("loop", "Running until the user's criterion holds: "+criterion)
			if k.emitter != nil {
				k.emitter.EmitTaskUpdate("agentic-loop", "contract",
					"stop condition: "+criterion)
			}
		}

		if contract == nil && k.shouldPlanForLoop(userGoal, complexity, hasProjectGuidance) {
			depth := decidePlanDepth(userGoal, complexity, hasProjectGuidance, k.planDepthCfg())
			if g, perr := k.deepPlan(ctx, k.injectRecall(userGoal, recallBlock), depth); perr == nil {
				g.Goal = userGoal
				planGraph = g
				contract = k.contractFor(g)
				k.log("loop", fmt.Sprintf("Loop bound to a %d-task plan with %d acceptance criteria",
					len(g.Nodes), len(contract.Criteria)))
				if k.emitter != nil {
					k.emitter.EmitDAGUpdate(g.ToDAG().Summary())
				}
			} else {
				// A planner failure must not cost the user their task. Fall
				// through to an unplanned loop — weaker stop condition, same
				// work.
				k.log("loop", "Planning failed: "+perr.Error()+" — looping without acceptance criteria")
			}
		}

		loopRes, err := k.agenticLoop.RunWithContract(ctx, k.injectRecall(userGoal, recallBlock), history, contract)
		if err != nil {
			k.storeEpisodic(userGoal, "", nil, false, recallBlock, nil)
			return "", err
		}
		output := loopRes.Output

		// Attach the evidence. A run that says it succeeded and shows the
		// commands it passed is a different claim from one that only says so.
		if planGraph != nil {
			markGraphFrom(planGraph, loopRes.Verdict, loopRes.Completed)
			k.mu.Lock()
			k.lastRunPlan = planGraph
			k.mu.Unlock()
			if summary := acceptanceSummary(planGraph); summary != "" {
				output += summary
			}
		}

		// Post-loop consensus: non-primary models review the loop's answer and
		// the primary synthesises. Extra calls purely for polish, so it runs
		// only when the request explicitly asked for consensus — the /consensus
		// verb, or consensus routing — rather than from a standing setting.
		if k.router.GetMode() == core.RouteConsensus && k.router.ModelCount() > 1 {
			k.log("consensus", "Running post-agentic consensus synthesis")
			if refined, cerr := k.runConsensusOnOutput(ctx, userGoal, output, loopRes.ToolTrace); cerr == nil {
				output = refined
			} else {
				k.log("consensus", "Consensus synthesis failed: "+cerr.Error()+" — using agentic output")
			}
		}

		// Record to STM + episodic/learning/audit/KG so the rest of the system
		// sees the task just like a DAG execution.
		//
		// The outcome and the tool list both come from the loop, and neither
		// used to: this recorded success=true with a nil result set, so a run
		// that gave up was stored as a short, successful, TOOL-FREE answer and
		// the cache happily replayed that abort text for the next request.
		// Both halves mattered — the honest flag, and the tools, without which
		// the cache's "never replay tool-using tasks" guard saw nothing to
		// guard against.
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
		loopResult := &core.SubAgentResult{
			Role:      core.RoleWorker,
			Goal:      userGoal,
			Output:    output,
			Success:   loopRes.Completed,
			ToolCalls: loopRes.ToolCalls,
		}
		k.recordOutcome(userGoal, output, []*core.SubAgentResult{loopResult},
			loopRes.Completed, "agentic-loop", 0, recallBlock)
		if k.emitter != nil {
			k.emitter.EmitFinalOutput(output)
		}
		return output, nil
	}

	// Step 4: Decide — trivial task or needs decomposition?
	if k.isTrivial(userGoal, complexity) {
		k.log("plan", "Task is trivial — executing directly")
		return k.executeDirect(ctx, userGoal, recallBlock)
	}

	// Step 5: Plan — the unified planning phase (deep_planner.go). The
	// PRIMARY model decomposes the goal into a plan.Graph at a depth chosen
	// by the adaptive governor (planning effort is itself cost-governed:
	// light = one call, deep = decompose + adversarial self-review). The
	// recall block is injected into the planner's context, but the graph
	// keeps the clean userGoal for episodic memory and re-planning.
	k.mu.Lock()
	planDepthCfg := k.cfg.PlanDepth
	planApprovalCfg := k.cfg.PlanApproval
	k.mu.Unlock()
	depth := decidePlanDepth(userGoal, complexity, hasProjectGuidance, planDepthCfg)
	k.log("plan", fmt.Sprintf("Planning phase: %s depth (complexity %d)", depth, complexity))
	g, err := k.deepPlan(ctx, k.injectRecall(userGoal, recallBlock), depth)
	if err != nil {
		// Fallback: execute directly if planning fails
		k.log("plan", "Planning failed: "+err.Error()+" — falling back to direct execution")
		return k.executeDirect(ctx, userGoal, recallBlock)
	}
	g.Goal = userGoal

	// Step 5.5: Approval gate — pause for the user's approve/revise/reject
	// when the policy requires it ("always", or "auto" for deep plans). The
	// proposal is the turn's answer; the decision arrives next turn and is
	// handled at Step 1.1.
	if planApprovalRequired(planApprovalCfg, depth) {
		preview := k.previewWithImpact(g)
		k.setPendingPlan(g)
		k.log("plan", fmt.Sprintf("Plan awaiting user approval: %d task(s)", len(g.Nodes)))
		k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: preview})
		if k.emitter != nil {
			k.emitter.EmitTaskUpdate("planner", "awaiting-approval",
				fmt.Sprintf("Plan with %d task(s) awaiting approval", len(g.Nodes)))
			k.emitter.EmitFinalOutput(preview)
		}
		return preview, nil
	}

	// Steps 6-10: execute the graph (DAG run → merge → verify → record →
	// emit) — see executePlannedGraph in dag_executor.go.
	return k.executePlannedGraph(ctx, g, recallBlock)
}
