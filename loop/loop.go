// Package loop implements the optional "looping technology" — an explicit
// agentic Sense-Think-Act (OODA / ReAct) execution loop.
//
// Unlike the kernel's default single-pass DAG decomposition (Plan → Execute →
// Merge), the ReAct loop runs a continuous iterative cycle:
//
//  1. THINK  (Orient/Decide) — the LLM reasons about the current state and
//     either produces a final answer or requests one or more tool calls.
//  2. ACT    — requested tools are executed via the tool registry.
//  3. OBSERVE — tool output is captured, truncated (context-drift
//     mitigation), and appended to the conversation history.
//  4. LOOP   — repeat until the LLM emits a final answer or MaxLoops is hit.
//
// This mirrors the design described in looping_tech: max-iteration limit to
// break stuck loops, observation truncation to mitigate context-window drift,
// and an explicit stop condition (no tool calls → done).
//
// The loop is OPTIONAL: it is enabled by the user from the Settings tab
// (Config.AgenticLoop) and hot-toggled at runtime via Kernel.SetAgenticLoop.
package loop

import (
	"context"
	"fmt"
	"github.com/darkcode/internal/strutil"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
	"github.com/darkcode/observability"
	"github.com/darkcode/agents"
)

// MaxObservationLen caps the size of a single tool's observation text that is
// fed back into the conversation history. This is the "context window drift"
// mitigation from the doc: raw tool output (verbose port scans, large file
// reads, huge HTML blobs) can drown out the original objective, so each
// observation is truncated before being appended.
const MaxObservationLen = 4000

// DefaultMaxLoops is the safety ceiling when none is configured. The doc
// suggests ~20; we keep a slightly higher default but still bounded.
const DefaultMaxLoops = 20

// ReActLoop is the agentic execution loop. It is constructed once by the
// orchestrator kernel and re-used per Execute call when AgenticLoop is on.
type ReActLoop struct {
	router   core.ModelRouter
	registry core.ToolRegistry
	emitter  *ui.EventEmitter
	maxLoops int
}

// New creates a ReAct loop wired to the model router, tool registry, and event
// emitter. maxLoops <= 0 falls back to DefaultMaxLoops.
func New(rtr core.ModelRouter, reg core.ToolRegistry, emitter *ui.EventEmitter, maxLoops int) *ReActLoop {
	if maxLoops <= 0 {
		maxLoops = DefaultMaxLoops
	}
	return &ReActLoop{
		router:   rtr,
		registry: reg,
		emitter:  emitter,
		maxLoops: maxLoops,
	}
}

// SetMaxLoops updates the iteration ceiling at runtime (hot config from UI).
func (l *ReActLoop) SetMaxLoops(n int) {
	if n > 0 {
		l.maxLoops = n
	}
}

// Result is the outcome of a ReAct loop run. Output is the agent's final
// answer; ToolTrace is a concise, human-readable log of every tool call that
// was executed plus its real result. Callers that refine the answer with a
// downstream LLM step (e.g. consensus synthesis) MUST pass ToolTrace into
// that step so the refiners know the tools actually ran — otherwise a
// "skeptic" model can hallucinate that the agent cannot take action, even
// though the tools executed successfully.
type Result struct {
	Output    string
	ToolTrace string
}

// Run executes the Sense-Think-Act loop for the given goal and returns the
// agent's final answer along with a trace of the tools it executed.
func (l *ReActLoop) Run(ctx context.Context, goal string) (*Result, error) {
	ctx, span := observability.StartSpan(ctx, "agentic-loop")
	defer span.End()

	if l.router == nil {
		return nil, fmt.Errorf("agentic loop: router not configured")
	}
	// Route to the coding tier (the capable general-purpose tier). Complexity
	// is assessed from the goal so the router can still pick the right model.
	complexity := router.AssessComplexity(goal)
	client, modelName, err := l.router.Route(core.ModelTierCoding, complexity, goal)
	if err != nil {
		return nil, fmt.Errorf("agentic loop: model routing failed: %w", err)
	}

	// Assemble the initial conversation: a ReAct system prompt + the goal.
	messages := []core.Message{
		{Role: core.RoleSystem, Content: l.systemPrompt()},
		{Role: core.RoleUser, Content: goal},
	}

	if l.emitter != nil {
		l.emitter.EmitTaskUpdate("agentic-loop", "started",
			fmt.Sprintf("ReAct loop beginning (max %d iterations, model %s)", l.maxLoops, modelName))
	}

	// Constructed once per Run() — not per iteration — since it's re-entered
	// on every failed-verification `continue` below and rebuilding it (with
	// its 7-stage language detection) on every stop-condition check was
	// wasted work.
	verifier := agents.NewVerificationPipeline(l.router, l.emitter, "")

	var allToolCalls []core.ToolCall
	var trace strings.Builder
	// stuckFails tracks consecutive failures of the same (tool, args) call.
	// When the agent repeats a failing call, we nudge it to change strategy;
	// after one more repeat we break early so a stuck loop can't burn the
	// entire iteration budget on the same error.
	stuckFails := make(map[string]int)
	start := time.Now()

	// ── The loop ──────────────────────────────────────────────────────────
	for iteration := 1; iteration <= l.maxLoops; iteration++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if l.emitter != nil {
			l.emitter.EmitTaskUpdate("agentic-loop", "thinking",
				fmt.Sprintf("iteration %d/%d — reasoning", iteration, l.maxLoops))
		}

		// ── 1. THINK (Orient/Decide) ──────────────────────────────────────
		temp := 0.7
		req := &core.CompletionRequest{
			Model:       modelName,
			Messages:    messages,
			Tools:       l.registry.LLMSchemas().([]llm.ToolSchema),
			Temperature: &temp,
		}

		resp, err := client.ChatCompletionStream(ctx, req, &core.StreamCallbacks{
			OnContent: func(chunk string) {
				if l.emitter != nil {
					l.emitter.Emit(core.EventTaskUpdate, chunk,
						ui.WithTaskID("agentic-loop"), ui.WithStatus("streaming"))
				}
			},
			OnToolCall: func(tc core.ToolCall) {
				if l.emitter != nil {
					l.emitter.EmitToolExecution(tc.Function.Name, "requested", tc.Function.Arguments)
				}
			},
		})
		if err != nil {
			return nil, fmt.Errorf("agentic loop iteration %d: %w", iteration, err)
		}
		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("agentic loop iteration %d: empty response", iteration)
		}

		msg := resp.Choices[0].Message
		messages = append(messages, core.Message{
			Role:      core.RoleAssistant,
			Content:   msg.Content,
			ToolCalls: msg.ToolCalls,
		})

		// ── 2. STOP CONDITION — no tool calls means final answer ─────────
		if len(msg.ToolCalls) == 0 {
			final := msg.Content

			// Verification Gate
			vResult, _ := verifier.Verify(ctx, goal, final, nil)
			if !vResult.Passed && len(vResult.Issues) > 0 {
				// Self-correct by appending issues to context
				issuePrompt := fmt.Sprintf("Verification failed with issues:\n%s\nPlease correct your output.", strings.Join(vResult.Issues, "\n"))
				messages = append(messages, core.Message{
					Role:    core.RoleSystem,
					Content: issuePrompt,
				})
				if l.emitter != nil {
					l.emitter.EmitTaskUpdate("agentic-loop", "verifying", "Verification failed, forcing self-correction")
				}
				continue // loop back and fix it
			}

			if l.emitter != nil {
				l.emitter.EmitTaskUpdate("agentic-loop", "complete",
					fmt.Sprintf("ReAct loop finished after %d iteration(s) in %s", iteration, time.Since(start).Round(time.Millisecond)))
			}
			_ = allToolCalls // collected for the caller via events
			return &Result{Output: final, ToolTrace: trace.String()}, nil
		}

		// ── 3. ACT — execute the requested tools ─────────────────────────
		allToolCalls = append(allToolCalls, msg.ToolCalls...)
		resultsi := l.registry.DispatchAll(ctx, msg.ToolCalls)
		results, ok := resultsi.([]tools.DispatchResult)
		if !ok {
			return nil, fmt.Errorf("agentic loop iteration %d: unexpected tool result type", iteration)
		}

		// ── 4. OBSERVE — append (truncated) tool output to history ───────
		for _, r := range results {
			obs := formatObservation(r)
			if l.emitter != nil {
				l.emitter.EmitToolExecution(r.Name, "completed", obs)
			}
			// Record the real tool outcome in the trace so any downstream
			// refiner (consensus synthesis) is grounded in what actually
			// happened — it cannot then claim the agent lacks tool access.
			fmt.Fprintf(&trace, "%d. %s(%s) → %s\n", len(allToolCalls), r.Name,
				argSummary(r.CallID, msg.ToolCalls), traceSnippet(obs))
			messages = append(messages, core.Message{
				Role:       core.RoleTool,
				Content:    strutil.Truncate(obs, MaxObservationLen),
				ToolCallID: r.CallID,
				Name:       r.Name,
			})
		}

		// ── 4.5 REFLECT + STUCK DETECTION ───────────────────────────────
		// Emit a concise per-iteration reflection so the UI live-trace shows
		// what just happened, and detect repeated failing calls so a stuck
		// loop can't waste the whole budget on the same error.
		acted := make([]string, 0, len(results))
		for _, r := range results {
			acted = append(acted, r.Name)
			if r.Result != nil && !r.Result.Success {
				key := callKey(r.Name, r.CallID, msg.ToolCalls)
				stuckFails[key]++
				if stuckFails[key] == 3 {
					messages = append(messages, core.Message{
						Role:    core.RoleSystem,
						Content: "You are stuck: " + r.Name + " has failed 3× with the same arguments. Change your approach or give the final answer now.",
					})
					if l.emitter != nil {
						l.emitter.EmitTaskUpdate("agentic-loop", "stuck",
							fmt.Sprintf("iteration %d: %s repeated failing — nudging strategy change", iteration, r.Name))
					}
				}
				if stuckFails[key] >= 4 {
					if l.emitter != nil {
						l.emitter.EmitTaskUpdate("agentic-loop", "aborted",
							fmt.Sprintf("iteration %d: %s failed %d× — aborting loop to avoid waste", iteration, r.Name, stuckFails[key]))
					}
					return &Result{Output: "The agent got stuck repeatedly calling " + r.Name + " and stopped to avoid wasting iterations.\n\n" + bestPartial(messages) + "\n\n_(agentic loop aborted: repeated tool failure)_", ToolTrace: trace.String()}, nil
				}
			} else {
				// A success resets the stuck counter for that call signature.
				delete(stuckFails, callKey(r.Name, r.CallID, msg.ToolCalls))
			}
		}
		if l.emitter != nil {
			l.emitter.EmitTaskUpdate("agentic-loop", "reflect",
				fmt.Sprintf("iteration %d/%d complete — acted: %s", iteration, l.maxLoops, strings.Join(acted, ", ")))
		}
	}

	// Max loops reached without a final answer — return the best-effort last
	// assistant content (if any) so the user gets something useful, and emit a
	// max-reached notice.
	if l.emitter != nil {
		l.emitter.EmitTaskUpdate("agentic-loop", "max_reached",
			fmt.Sprintf("ReAct loop hit max iterations (%d) — returning last partial answer", l.maxLoops))
	}
	if partial := bestPartial(messages); partial != "" {
		return &Result{Output: partial + "\n\n_(agentic loop reached the max iteration limit)_", ToolTrace: trace.String()}, nil
	}
	return nil, fmt.Errorf("agentic loop reached max iterations (%d) without a final answer", l.maxLoops)
}

// systemPrompt returns the ReAct instruction set given to the model. It tells
// the LLM to reason step-by-step and use the provided tools, and to stop
// (return a plain answer with no tool calls) once the goal is achieved.
func (l *ReActLoop) systemPrompt() string {
	var b strings.Builder
	b.WriteString("You are DarkCode running in Agentic Loop (ReAct) mode — an autonomous " +
		"agent that takes REAL action in the world via tools. You are NOT a chatbot that " +
		"only talks; you DO things.\n\n")
	b.WriteString("EXECUTION CYCLE (repeat until the goal is met):\n")
	b.WriteString("  1. THOUGHT — reason about the current state and what to do next. Briefly state your plan.\n")
	b.WriteString("  2. ACTION — call one or more of the provided tools to gather information or change the world.\n")
	b.WriteString("  3. OBSERVATION — read each tool's result, then decide the next step.\n")
	b.WriteString("  4. STOP — when the goal is FULLY achieved, respond with your FINAL answer as plain text and DO NOT call any tool.\n")
	b.WriteString("     The absence of a tool call is the stop signal that ends the loop.\n\n")
	b.WriteString("RULES:\n")
	b.WriteString("- Call tools to ACT, not to ask permission. You already have permission to use the provided tools.\n")
	b.WriteString("- Verify the goal is truly met before stopping — e.g. after writing a file, read it back to confirm.\n")
	b.WriteString("- If a tool errors, READ the error and adapt your approach rather than repeating the same failing call.\n")
	b.WriteString("- If the request is ambiguous or lacks enough detail to act safely, stop and ask a concise clarifying question instead of inventing missing requirements.\n")
	b.WriteString("- If the same tool call fails repeatedly or produces no new information, stop and report the blocker rather than burning more turns.\n")
	b.WriteString("- Prefer parallel tool calls when actions are independent (they execute concurrently).\n")
	b.WriteString("- Be concise in intermediate thoughts; reserve detail for the final answer.\n")
	b.WriteString("- If a tool result says \"permission denied by user\" with feedback, honour that steer and change your approach accordingly.\n")
	return b.String()
}

// callKey builds a signature for a (tool, arguments) pair so the stuck
// detector can recognize the SAME failing call being repeated. It looks up
// the arguments by callID from the LLM-emitted tool calls.
func callKey(tool, callID string, calls []core.ToolCall) string {
	args := ""
	for _, c := range calls {
		if c.ID == callID {
			args = c.Function.Arguments
			break
		}
	}
	return tool + "|" + args
}

// bestPartial returns the last assistant text in the conversation, for use
// when the loop aborts early (stuck/max-iterations) so the user still gets
// whatever the agent last produced.
func bestPartial(messages []core.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == core.RoleAssistant {
			if s := messages[i].ContentString(); strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// argSummary returns a compact rendering of the arguments for a tool call
// identified by callID, so the trace shows what was invoked (not just the
// name). It scans the LLM-emitted tool calls for the matching ID.
func argSummary(callID string, calls []core.ToolCall) string {
	for _, c := range calls {
		if c.ID == callID {
			s := strings.TrimSpace(c.Function.Arguments)
			if len(s) > 120 {
				s = s[:120] + "…"
			}
			return s
		}
	}
	return ""
}

// traceSnippet shortens a tool observation for the human-readable trace.
func traceSnippet(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// formatObservation renders a DispatchResult into the observation text that
// gets fed back to the LLM.
func formatObservation(r tools.DispatchResult) string {
	if r.Result == nil {
		return "(tool returned no result)"
	}
	if !r.Result.Success && r.Result.Error != "" {
		return "Error: " + r.Result.Error
	}
	if r.Result.Output != "" {
		return r.Result.Output
	}
	return "(tool completed with no output)"
}

// truncate caps a string to n characters with an ellipsis marker, the
// context-drift mitigation from the looping tech doc.
