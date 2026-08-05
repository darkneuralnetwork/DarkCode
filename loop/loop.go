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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/darkcode/internal/strutil"

	"github.com/darkcode/agents"
	"github.com/darkcode/compression"
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/observability"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
)

// nudgeRole is the role for a steer injected part-way through a conversation —
// a self-evaluation, a stuck-loop warning, a budget notice.
//
// It is a user turn rather than a system one. Gemini folds system messages into
// systemInstruction at the top of the request, so one appearing after a tool
// response leaves the following tool call with nothing valid before it, and the
// request is rejected outright: "function call turn must come immediately after
// a user turn or after a function response turn". OpenAI tolerates the system
// form, which is why this survived — but a user turn is accepted everywhere and
// reads the same to the model.
const nudgeRole = core.RoleUser

// MaxObservationLen caps the size of a single tool's observation text that is
// fed back into the conversation history. This is the "context window drift"
// mitigation from the doc: raw tool output (verbose port scans, large file
// reads, huge HTML blobs) can drown out the original objective, so each
// observation is truncated before being appended.
const MaxObservationLen = 4000

// DefaultMaxLoops is the ceiling on turns that ACTED. It is a backstop, not a
// preference — completion is decided by acceptance checks, and this only
// catches the case those cannot: an agent spinning productively-looking
// forever. It used to be a config field defaulting to 3, which was low enough
// that a single verification failure and one self-evaluation nudge spent the
// whole allowance before any second piece of work happened.
const DefaultMaxLoops = 25

// maxCorrections bounds rounds spent re-checking an answer the agent already
// considered final — a failed verification, or a self-evaluation that found
// the goal unmet. It is deliberately separate from maxLoops: these rounds are
// the loop arguing with itself, not doing work, and charging them to the same
// budget meant a task could exhaust its entire allowance without completing a
// second real step.
const maxCorrections = 8

// maxEvalFailures bounds how many times a self-evaluation that could not RUN
// (transport error, exhausted quota) may hold the loop open. Failing closed is
// right — an unrunnable check is not evidence the work is done — but a model
// that is simply unreachable would otherwise spin here forever.
const maxEvalFailures = 2

// loopHistoryBudgetBytes bounds how much prior conversation (from the
// caller's STM) is folded into a Run() call, keeping the most recent
// messages when the budget is exceeded. This is what gives the loop real
// conversation continuity (local-first upgrade §7 Fix C): previously every
// Run() started a brand-new 2-message conversation with zero memory of what
// it was doing, so a follow-up "continue" had nothing to continue.
const loopHistoryBudgetBytes = 6000

// ReActLoop is the agentic execution loop. It is constructed once by the
// orchestrator kernel and re-used per Execute call when AgenticLoop is on.
type ReActLoop struct {
	router   core.ModelRouter
	registry core.ToolRegistry
	emitter  *ui.EventEmitter
	maxLoops int
	// budget, when set, is consulted before each acting turn and stops the run
	// when it reports the spend cap reached.
	//
	// The cap used to be checked once, before the request started. A single
	// request can then make twenty-five iterations plus planning, consensus
	// fan-out and sub-agent calls, so a limit was checked once and then
	// exceeded several times over inside the run it was meant to bound.
	budget func() error
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

// SetBudgetCheck installs a per-iteration spend check. nil disables it.
//
// It returns an error rather than a bool so the reason reaches the user: "the
// run stopped" without saying why reads as the agent giving up.
func (l *ReActLoop) SetBudgetCheck(fn func() error) { l.budget = fn }

// BudgetCheckInstalled reports whether a spend check is wired. It exists so a
// caller can prove the wiring rather than assume it: the check is installed on
// the loop the kernel already holds, so a reordering that built the loop later
// would silently leave the cap as a once-per-request gate.
func (l *ReActLoop) BudgetCheckInstalled() bool { return l.budget != nil }

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

	// Completed reports whether the loop reached a genuine final answer, as
	// opposed to giving up (repeated tool failure) or running out of
	// iterations. The caller MUST record the task's outcome from this rather
	// than assuming success.
	//
	// It exists because assuming success was a real, user-visible bug: the
	// kernel recorded every loop run with success=true, so "the agent got
	// stuck repeatedly calling write_file and stopped" was written to
	// episodic memory as a short, successful, tool-free answer — the most
	// replayable shape there is. The answer cache then served that abort text
	// back for later requests, and the agent appeared to respond to a new
	// instruction with a previous error.
	Completed bool

	// ToolCalls is every tool the loop actually invoked. The caller needs it
	// to record what the task used; passing nil made a heavily tool-using run
	// look tool-free to the cache's "never replay tool-using tasks" guard.
	ToolCalls []core.ToolCall

	// Verdict is the final contract check, when a contract was supplied and
	// enforceable. Verdict.Proven() distinguishes "the acceptance criteria were
	// run and held" from "nothing contradicted the claim" — the caller records
	// the first as a success and should not record the second as one.
	Verdict Verdict

	// Stuck reports that the loop gave up because the same call kept failing,
	// as opposed to running out of iterations or finishing.
	//
	// The distinction is what lets the caller escalate instead of apologise:
	// repeating a call that has already failed four times is the one response
	// guaranteed not to work, whereas breaking the task up might.
	Stuck bool
}

// RunWithContract is Run plus a caller-supplied definition of done. When the
// contract is enforceable the loop stops on evidence rather than on the model's
// opinion of its own work; see contract.go.
func (l *ReActLoop) RunWithContract(ctx context.Context, goal string, history []core.Message, contract *Contract) (*Result, error) {
	return l.run(ctx, goal, history, contract)
}

// Run executes the Sense-Think-Act loop for the given goal and returns the
// agent's final answer along with a trace of the tools it executed. history
// is the caller's prior conversation (STM) — nil/empty for a genuinely fresh
// task, non-empty when this is a follow-up (e.g. "continue") so the loop
// knows what it was doing rather than starting from zero every time (local-
// first upgrade §7 Fix C). Truncated to loopHistoryBudgetBytes, keeping the
// most recent messages, so a long conversation can't blow out the context
// window on every single loop turn.
func (l *ReActLoop) Run(ctx context.Context, goal string, history []core.Message) (*Result, error) {
	return l.run(ctx, goal, history, nil)
}

func (l *ReActLoop) run(ctx context.Context, goal string, history []core.Message, contract *Contract) (*Result, error) {
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

	// Assemble the initial conversation: a ReAct system prompt + prior
	// history (if any, continuity) + the goal.
	messages := []core.Message{{Role: core.RoleSystem, Content: l.systemPrompt() + contract.brief()}}
	messages = append(messages, truncateHistory(history, loopHistoryBudgetBytes)...)
	messages = append(messages, core.Message{Role: core.RoleUser, Content: goal})

	if l.emitter != nil {
		l.emitter.EmitTaskUpdate("agentic-loop", "started",
			fmt.Sprintf("ReAct loop beginning (max %d iterations, model %s)", l.maxLoops, modelName))
	}

	// Constructed once per Run() — not per iteration — since it's re-entered
	// on every failed-verification `continue` below and rebuilding it (with
	// its 7-stage language detection) on every stop-condition check was
	// wasted work.
	//
	// The workspace comes from the request context. It used to be "", which
	// made the language detector inspect the PROCESS's working directory
	// instead of the user's project: on any machine where DarkCode was started
	// from a Go checkout, every stop-condition check shelled out gofmt, go
	// build, go test and go vet against the wrong tree. That was slow, and any
	// unrelated breakage there failed verification and spent one of the loop's
	// few iterations "self-correcting" work it had never done.
	verifier := agents.NewVerificationPipeline(l.router, l.emitter, core.WorkspaceFrom(ctx))

	var allToolCalls []core.ToolCall
	var trace strings.Builder
	// stuckFails tracks consecutive failures of the same (tool, args) call.
	// When the agent repeats a failing call, we nudge it to change strategy;
	// after one more repeat we break early so a stuck loop can't burn the
	// entire iteration budget on the same error.
	stuckFails := make(map[string]int)
	// refitDone guards the one-shot context-overflow recovery so a persistent
	// overflow can't spin the loop forever (single hard refit, then surface).
	refitDone := false
	start := time.Now()

	// Two separate budgets, because they pay for different things and used to
	// share one counter.
	//
	// iteration counts turns that ACTED — a THINK that produced tool calls.
	// corrections counts rounds spent re-checking an answer the agent already
	// considered final: a failed verification, or a self-evaluation that said
	// the goal wasn't met. Previously both ran through the same `iteration++`,
	// so with the shipped default of 3 a single verification failure and one
	// self-eval nudge consumed the entire budget before the agent had done any
	// second piece of work. The task then ended at "max iterations" having
	// worked once.
	//
	// evalFailures is a third, much smaller budget: it bounds how many times a
	// self-eval that could not RUN (transport error, exhausted quota) may hold
	// the loop open. Without it, failing closed on an unreachable model would
	// spin.
	iteration, corrections, evalFailures := 0, 0, 0

	// unlocked holds tools the model asked for that the relevance filter had
	// not offered. Re-offering them for the rest of the run is what makes the
	// filter a cost saving rather than a capability cut: guessing wrong costs
	// one turn, and the model does not have to keep rediscovering the tool.
	unlocked := map[string]bool{}

	// verdict holds the most recent contract check, so the final Result can
	// report what was actually proven rather than only whether the loop chose
	// to stop.
	var verdict Verdict

	// budgetStop records a spend cap reached mid-run, so the final answer can
	// say the work was cut short rather than presenting itself as complete.
	var budgetStop error

	// ── The loop ──────────────────────────────────────────────────────────
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if iteration >= l.maxLoops {
			break
		}
		// Stop on a reached spend cap rather than running the remaining
		// iterations. The work done so far is still returned: losing it would
		// spend the budget and hand back nothing.
		if l.budget != nil {
			if err := l.budget(); err != nil {
				budgetStop = err
				if l.emitter != nil {
					l.emitter.EmitTaskUpdate("agentic-loop", "budget", err.Error())
				}
				break
			}
		}

		if l.emitter != nil {
			l.emitter.EmitTaskUpdate("agentic-loop", "thinking",
				fmt.Sprintf("iteration %d/%d — reasoning", iteration+1, l.maxLoops))
		}

		// ── 1. THINK (Orient/Decide) ──────────────────────────────────────
		temp := 0.7
		// Chat (read-only) requests offer only read-only tools so the model is
		// never given a write/execute tool to call.
		var schemas []llm.ToolSchema
		if core.IsReadOnlyTools(ctx) {
			schemas = l.registry.LLMSchemasReadOnly().([]llm.ToolSchema)
		} else {
			schemas = l.registry.LLMSchemas().([]llm.ToolSchema)
		}
		// Send only the tools this goal could plausibly use. The whole registry
		// is ~2,047 tokens on EVERY iteration, so a long run spends more
		// describing tools than doing work. Anything the model asks for anyway
		// is unlocked for the rest of the run (see below), which keeps a wrong
		// guess to one turn rather than a failed task.
		schemas = tools.RelevantSchemas(goal, schemas, unlocked)
		// Hard context-fit guarantee: messages grow with each iteration's tool
		// output, so fit to the RECEIVING client's effective window right
		// before dispatch (Part 3 contract). Prevents the "context window
		// exceeded" fatal on long local-model loops.
		messages = compression.FitClient(messages, client, 0, len(schemas))
		req := &core.CompletionRequest{
			Model:       modelName,
			Messages:    messages,
			Tools:       schemas,
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
			// Recovery ladder: a context overflow past the FitClient estimate
			// (tokenizer drift) shrinks hard to 75% of the window and retries
			// the same iteration once, instead of aborting the whole task.
			if errors.Is(err, core.ErrContextTooLong) && !refitDone {
				refitDone = true
				window := client.ModelInfo().Context
				if window <= 0 {
					window = compression.DefaultContextWindow
				}
				messages = compression.FitToWindow(messages, window*3/4, 0)
				continue // a refit is not a work turn; iteration is unchanged
			}
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
			if !vResult.Passed && len(vResult.Issues) > 0 && corrections < maxCorrections {
				corrections++
				// Self-correct by appending issues to context
				issuePrompt := fmt.Sprintf("Verification failed with issues:\n%s\nPlease correct your output.", strings.Join(vResult.Issues, "\n"))
				// Mid-conversation steers are user turns; see nudgeRole below.
				messages = append(messages, core.Message{
					Role:    nudgeRole,
					Content: issuePrompt,
				})
				if l.emitter != nil {
					l.emitter.EmitTaskUpdate("agentic-loop", "verifying",
						fmt.Sprintf("Verification failed (%d/%d) — forcing self-correction", corrections, maxCorrections))
				}
				continue // loop back and fix it
			}

			// completionVerified records whether a check actually RAN and
			// agreed the goal was met. It is what the caller records as the
			// task's outcome, so an answer that could never be verified —
			// because the budget ran out, or the checker was unreachable — is
			// returned to the user but NOT written to memory as a success.
			completionVerified := false

			// ── ACCEPTANCE GATE ──────────────────────────────────────────
			// When the caller supplied an enforceable contract, completion is
			// decided by running the criteria, not by asking the model whether
			// it is happy with its own work. A failing check comes back as the
			// command's actual output, which is a far better correction signal
			// than any phrasing of "try again" — the compiler already said
			// precisely what is wrong.
			if contract.enforceable() {
				verdict = contract.Verify(ctx)
				if !verdict.Passed && verdict.Checked > 0 && corrections < maxCorrections {
					corrections++
					messages = append(messages, core.Message{
						Role: nudgeRole,
						Content: "The acceptance checks for this task FAILED. This is the real output:\n\n" +
							strutil.Truncate(verdict.Evidence, MaxObservationLen) +
							"\n\nFix the cause and continue. Do not give a final answer until these pass.",
					})
					if l.emitter != nil {
						l.emitter.EmitTaskUpdate("agentic-loop", "acceptance",
							fmt.Sprintf("Acceptance checks failed (%d/%d) — correcting", corrections, maxCorrections))
					}
					continue
				}
				if verdict.Proven() {
					// Evidence, not opinion. Skip self-evaluation entirely —
					// asking a model to second-guess a passing test suite adds
					// a call and can only make the answer worse.
					completionVerified = true
					if l.emitter != nil {
						l.emitter.EmitTaskUpdate("agentic-loop", "acceptance",
							fmt.Sprintf("Acceptance checks passed (%d checked)", verdict.Checked))
					}
				}
			}

			// Self-evaluation. This is the fallback for goals with nothing
			// machine-checkable about them, and it is the only thing standing
			// between "the model stopped calling tools" and "the goal is
			// actually met". It runs on every stop attempt rather than being
			// skipped on the final iteration as it used to be — the old guard
			// meant the last turn always declared itself done.
			// A read-only turn that called no tools has nothing to verify. Its
			// text IS the deliverable — there is no state change that could
			// disagree with it — so self-evaluation is the model grading its
			// own answer, for a call. Same reasoning as the acceptance skip
			// above: evidence, not opinion, and here there is no evidence to
			// have. This is what keeps a conversational question a single
			// call now that such questions get tools instead of being denied
			// them.
			//
			// Deliberately NOT extended to build turns. There, "answered
			// without calling anything" is precisely the failure self-eval
			// exists to catch.
			if len(allToolCalls) == 0 && core.IsReadOnlyTools(ctx) {
				completionVerified = true
			}

			if !completionVerified && corrections < maxCorrections {
				done, reason, ran := l.evaluateGoalCompletion(ctx, client, modelName, goal, final)
				switch {
				case !ran && evalFailures < maxEvalFailures:
					// The check could not run. Failing OPEN here is what made
					// the loop stop after one turn on an exhausted free-tier
					// quota: every self-eval 429'd, every 429 was read as
					// "done", and the task ended having barely started. Treat
					// an unrunnable check as "not yet verified" and keep
					// working, but only a couple of times — a permanently
					// unreachable model must not spin the loop.
					evalFailures++
					corrections++
					messages = append(messages, core.Message{
						Role: nudgeRole,
						Content: "The completion check could not run (" + reason + "). " +
							"Re-read the goal and confirm every part of it is done; if anything is outstanding, continue working.",
					})
					if l.emitter != nil {
						l.emitter.EmitTaskUpdate("agentic-loop", "self-eval",
							fmt.Sprintf("Completion check unavailable (%d/%d): %s", evalFailures, maxEvalFailures, reason))
					}
					continue
				case ran && !done:
					corrections++
					messages = append(messages, core.Message{
						Role: nudgeRole,
						Content: "Self-evaluation: the goal is not yet fully met — " + reason +
							"\nContinue working; do not repeat steps you've already completed.",
					})
					if l.emitter != nil {
						l.emitter.EmitTaskUpdate("agentic-loop", "self-eval",
							"Self-evaluation found the goal incomplete: "+reason)
					}
					continue // loop back and keep working
				case ran && done:
					completionVerified = true
				}
				// The remaining case — the check could not run and its budget
				// is spent — deliberately leaves completionVerified false.
				// The user still gets the answer; memory does not get to
				// treat it as a confirmed success.
			}

			if l.emitter != nil {
				status := "complete"
				detail := fmt.Sprintf("ReAct loop finished after %d iteration(s) in %s",
					iteration, time.Since(start).Round(time.Millisecond))
				if !completionVerified {
					status = "unverified"
					detail += " — completion could not be verified"
				}
				l.emitter.EmitTaskUpdate("agentic-loop", status, detail)
			}
			return &Result{Output: final, ToolTrace: trace.String(),
				Completed: completionVerified, ToolCalls: allToolCalls, Verdict: verdict}, nil
		}

		// ── 3. ACT — execute the requested tools ─────────────────────────
		// This turn is doing work, so it is what the iteration budget is for.
		iteration++
		for _, tc := range msg.ToolCalls {
			unlocked[tc.Function.Name] = true
		}
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
			toolName := r.Name
			if toolName == "" {
				toolName = "unknown_tool"
			}
			messages = append(messages, core.Message{
				Role: core.RoleTool,
				// Oversized results are offloaded, not truncated: the model
				// gets a head/tail preview plus a read_result handle, so the
				// remainder stays reachable instead of being discarded.
				Content:    l.registry.ObserveResult(toolName, obs),
				ToolCallID: r.CallID,
				Name:       toolName,
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
						Role:    nudgeRole,
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
					return &Result{
						Output:    "The agent got stuck repeatedly calling " + r.Name + " and stopped to avoid wasting iterations.\n\n" + bestPartial(messages) + "\n\n_(agentic loop aborted: repeated tool failure)_",
						ToolTrace: trace.String(),
						Completed: false,
						ToolCalls: allToolCalls,
						Verdict:   verdict,
						Stuck:     true,
					}, nil
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
	// Why the loop ended decides what the user is told. A run cut short by the
	// spend cap is not the same as one that ran out of iterations, and calling
	// it "max iterations" would send them to change the wrong setting.
	stopNote := fmt.Sprintf("_(agentic loop reached the max iteration limit)_")
	if budgetStop != nil {
		stopNote = "_(stopped: " + budgetStop.Error() + ")_"
	} else if l.emitter != nil {
		l.emitter.EmitTaskUpdate("agentic-loop", "max_reached",
			fmt.Sprintf("ReAct loop hit max iterations (%d) — returning last partial answer", l.maxLoops))
	}
	if partial := bestPartial(messages); partial != "" {
		return &Result{
			Output:    partial + "\n\n" + stopNote,
			ToolTrace: trace.String(),
			Completed: false,
			ToolCalls: allToolCalls,
			Verdict:   verdict,
		}, nil
	}
	if budgetStop != nil {
		return nil, budgetStop
	}
	return nil, fmt.Errorf("agentic loop reached max iterations (%d) without a final answer", l.maxLoops)
}

// selfEvalDoneMarker / selfEvalContinuePrefix are the structured response
// tokens evaluateGoalCompletion asks for — a fixed-prefix check rather than
// fragile prose parsing, and cheap (the model is asked for one short line,
// not a paragraph).
const (
	selfEvalDoneMarker     = "GOAL_STATUS: DONE"
	selfEvalContinuePrefix = "GOAL_STATUS: CONTINUE"
)

// evaluateGoalCompletion asks the model, in one cheap completion, whether
// its own final answer actually satisfies the original goal — the missing
// piece that makes the loop genuinely "self-directed... evaluates its own
// output against a defined goal" (the user's own definition of loop
// engineering) instead of stopping purely because it produced a response
// with no tool calls. Called once per Run() (only at the syntactic stop
// condition), never per-iteration, so it doesn't double the cost of every
// loop turn.
//
// It reports ran=false when the check could not be performed at all
// (transport error, empty response). The caller decides what to do about
// that; this function deliberately does NOT decide on its behalf.
//
// It used to answer a plain (done, reason) and report "done" on any error,
// empty response or unparseable content. That looked conservative and was the
// single biggest reason the loop stopped early: on a metered free tier whose
// daily quota is measured in tens, every self-eval call 429'd, every 429 read
// as "the goal is met", and a multi-step task ended after its first answer.
// "The check failed" and "the work is finished" are not the same fact and are
// no longer conflated.
//
// Unparseable CONTENT still means done: a model that answered but ignored the
// format is not evidence the goal is unmet, and guessing at free-form prose is
// how this gets worse.
func (l *ReActLoop) evaluateGoalCompletion(ctx context.Context, client core.LLMClient, model, goal, final string) (done bool, reason string, ran bool) {
	// This check is a format-following task, not a reasoning task: the answer
	// is one of two fixed strings. A tiny local model routinely returns prose
	// instead, which parses as neither marker and therefore as "done" — so
	// routing here to save a few cents quietly re-created the fail-open
	// behaviour this function was rewritten to remove. Use the model that is
	// already doing the work.
	temp := 0.0
	maxTok := 60
	req := &core.CompletionRequest{
		Model: model,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "You are a strict completion checker. Respond with EXACTLY one line: \"" +
				selfEvalDoneMarker + "\" if the answer below fully and completely satisfies the goal, or \"" +
				selfEvalContinuePrefix + ": <one short reason>\" if it does not. No other text, no explanation."},
			{Role: core.RoleUser, Content: fmt.Sprintf("GOAL: %s\n\nANSWER:\n%s\n\nDoes the answer fully satisfy the goal?", goal, final)},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}
	resp, err := client.ChatCompletion(ctx, req)
	if err != nil {
		return false, err.Error(), false
	}
	if len(resp.Choices) == 0 {
		return false, "the completion check returned an empty response", false
	}
	line := strings.TrimSpace(resp.Choices[0].Message.Content)
	if strings.HasPrefix(line, selfEvalContinuePrefix) {
		reason = strings.TrimSpace(strings.TrimPrefix(line, selfEvalContinuePrefix))
		reason = strings.TrimSpace(strings.TrimPrefix(reason, ":"))
		if reason == "" {
			reason = "the model did not give a specific reason"
		}
		return false, reason, true
	}
	// The DONE marker, or any response that doesn't match the CONTINUE
	// prefix (e.g. the model ignored the format). The check RAN, so this is
	// evidence, not a failure — and guessing at free-form prose is worse than
	// taking the answer at face value.
	return true, "", true
}

// truncateHistory keeps the most recent messages from history whose combined
// content fits within maxBytes, dropping the oldest first — the byte-budget
// truncation Run uses to fold prior conversation in without letting a long
// history blow out the context window on every loop turn. Mirrors the same
// keep-the-tail pattern orchestrator.truncateMid uses for plan/workflow
// injection.
func truncateHistory(history []core.Message, maxBytes int) []core.Message {
	if len(history) == 0 {
		return nil
	}
	total := 0
	start := 0
	for i := len(history) - 1; i >= 0; i-- {
		total += len(history[i].ContentString())
		if total > maxBytes {
			start = i + 1
			break
		}
	}
	return history[start:]
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
