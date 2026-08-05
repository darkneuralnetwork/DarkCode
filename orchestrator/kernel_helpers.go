package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/compression"
	"github.com/darkcode/core"
	"github.com/darkcode/ctxengine"
	"github.com/darkcode/llm"
	"github.com/darkcode/modelport"
	"github.com/darkcode/router"
	"github.com/darkcode/ui"
)

// ============================================================================
// TRIVIAL TASK DETECTION
// ============================================================================

func (k *Kernel) isTrivial(goal string, complexity int) bool {
	// Trivial = low complexity AND short prompt AND no multi-step indicators.
	// Ambiguous requests are handled before this branch, so they do not get
	// routed into speculative tool execution.
	if complexity > 5 {
		return false
	}
	if len(goal) > 300 {
		return false
	}

	multiStepIndicators := []string{
		" and then ", " after that ", " step by step",
		" first ", " second ", " third ",
		" multiple ", " simultaneously", " in parallel",
		" decompose", " break down", " plan",
	}
	goalLower := strings.ToLower(goal)
	for _, indicator := range multiStepIndicators {
		if strings.Contains(goalLower, indicator) {
			return false
		}
	}

	return true
}

// RouteAux is the single decision point for auxiliary LLM calls (loop
// self-eval, context rewrite, plan amend). It prefers a local tier only when
// UseLocalForAux is on, a local model is loaded, and promptTokens fits its
// window; otherwise it returns the cloud route. ok=false means no model is
// available, so with no local model it's a pure no-op on unchanged behavior.
func (k *Kernel) RouteAux(task string, promptTokens int) (client core.LLMClient, model string, ok bool) {
	if k.router == nil {
		return nil, "", false
	}
	if k.cfg.UseLocalForAux {
		for _, tier := range []core.ModelTier{core.ModelTierMediumLocal, core.ModelTierTinyLocal} {
			lc, lm, err := k.router.Route(tier, 0, task)
			if err != nil || lc == nil {
				continue
			}
			// Only use local if the prompt fits its effective window; a bigger
			// aux prompt goes to cloud rather than overflowing the local model.
			if w := lc.ModelInfo().Context; promptTokens <= 0 || w <= 0 || promptTokens <= w {
				return lc, lm, true
			}
		}
	}
	cc, cm, err := k.router.Route(core.ModelTierCoding, 0, task)
	if err != nil || cc == nil {
		return nil, "", false
	}
	return cc, cm, true
}

// primaryContextWindow returns the effective context window (tokens) of the
// model likely to serve this request, for the compression trigger. Falls back
// to cfg.ContextLength then 0 ("unknown"), on which the caller skips the token
// trigger and relies on the message-count one.
func (k *Kernel) primaryContextWindow() int {
	if k.router == nil {
		return k.cfg.ContextLength
	}
	client, _, err := k.router.Route(core.ModelTierCoding, 0, "")
	if err != nil || client == nil {
		return k.cfg.ContextLength
	}
	if w := client.ModelInfo().Context; w > 0 {
		return w
	}
	return k.cfg.ContextLength
}

// goalIntent classifies the user's message so the clarification gate fires
// only for genuinely vague action requests. The default is answerable; only a
// cold-start message with no actionable subject gates.
type goalIntent int

const (
	// intentQuestion — an interrogative; always answerable, never gated.
	intentQuestion goalIntent = iota
	// intentContinuation — an active conversation or project blueprint gives
	// the model context to interpret even a terse follow-up ("continue").
	intentContinuation
	// intentAction — a concrete actionable request (the default).
	intentAction
	// intentVagueAction — a cold-start message with no actionable subject
	// ("fix it", "help me", empty or ultra-short input).
	intentVagueAction
)

// interrogativeLeads marks a question by its first word even without a
// trailing "?" ("what is the name of usa president").
var interrogativeLeads = map[string]bool{
	"what": true, "who": true, "whom": true, "whose": true, "when": true,
	"where": true, "why": true, "which": true, "how": true,
	"is": true, "are": true, "was": true, "were": true,
	"do": true, "does": true, "did": true,
	"can": true, "could": true, "should": true, "would": true, "will": true,
}

// vagueActionPhrases only make a request vague when they are essentially the
// whole message — see the remainder check in classifyGoalIntent.
var vagueActionPhrases = []string{
	"fix it",
	"make it better",
	"improve this",
	"do something",
	"help me",
	"work on it",
	"handle it",
	"update it",
	"make it work",
}

// QueryIsInformational reports whether goal is a question / read-only request
// (no state change implied), used by the server to skip the plan/workflow
// amend for a turn that couldn't change the plan. Deterministic, no LLM call.
// Reuses the same interrogative detection as the clarification gate so both
// agree on what "just a question" means.
func QueryIsInformational(goal string) bool {
	return classifyGoalIntent(goal, nil, false) == intentQuestion
}

// classifyGoalIntent is deterministic — no LLM call, so the gate stays free
// and instant regardless of outcome.
func classifyGoalIntent(goal string, stm []core.Message, hasProjectGuidance bool) goalIntent {
	if HasActiveConversation(stm) || hasProjectGuidance {
		return intentContinuation
	}
	goal = strings.TrimSpace(goal)
	if len(goal) < 8 {
		return intentVagueAction
	}
	goalLower := strings.ToLower(goal)
	if strings.HasSuffix(goalLower, "?") {
		return intentQuestion
	}
	words := strings.Fields(goalLower)
	if interrogativeLeads[words[0]] {
		return intentQuestion
	}
	// A cold-start action needs a subject: a single word ("continue",
	// "deploy") names nothing to act on — structural, not a keyword list.
	if len(words) == 1 {
		return intentVagueAction
	}
	for _, phrase := range vagueActionPhrases {
		if strings.Contains(goalLower, phrase) {
			// Vague only when the phrase IS the message: "help me fix the
			// auth bug in login.go" has a subject and stays actionable
			// despite containing "help me".
			if len(goalLower)-len(phrase) < 12 {
				return intentVagueAction
			}
		}
	}
	return intentAction
}

func (k *Kernel) executeDirect(ctx context.Context, goal string, recallBlock string) (string, error) {
	// Use a single worker agent
	cfg := core.SubAgentConfig{
		Role:      core.RoleWorker,
		Goal:      k.injectRecall(goal, recallBlock),
		ModelTier: core.ModelTierCoding,
		MaxTurns:  k.cfg.MaxTurns,
	}

	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("direct", "planning", "Executing trivial task directly")
	}

	agent, err := k.factory.Spawn(ctx, cfg)
	if err != nil {
		return "", err
	}

	result, err := agent.Execute(ctx)
	if err != nil {
		k.storeEpisodic(goal, "", []*core.SubAgentResult{result}, false, recallBlock, nil)
		return "", err
	}

	// Complexity-gated post-completion verification: even the direct path
	// verifies its output when the task is meaty enough to warrant it. The
	// pipeline's stages are deterministic (gofmt/build/tests/style, each
	// gated by IsApplicable) so this costs no LLM calls; failures are
	// surfaced in the answer instead of silently logged.
	output := result.Output
	output = k.verifyOutput(ctx, goal, output, router.AssessComplexity(goal))

	if k.emitter != nil {
		k.emitter.EmitFinalOutput(output)
	}

	// Direct tasks that actually used tools can still yield a simple skill —
	// minSkillSuccess=1 folds that in, see recordOutcome's doc comment.
	//
	// The outcome is the agent's own, not a hardcoded true: recording a failed
	// run as successful is what let error text into the answer cache and get
	// replayed as though it were an answer.
	k.recordOutcome(goal, output, []*core.SubAgentResult{result}, result.Success, "direct", 1, recallBlock)
	return output, nil
}

// verifyComplexityMin is the complexity floor for post-completion
// verification on the direct path (the DAG path always verifies — a planned
// task is non-trivial by definition).
const verifyComplexityMin = 6

// verifyOutput runs the self-verification pipeline when the task complexity
// warrants it, emits the outcome, and appends any found issues to the output
// so the user sees verification results instead of a silent log line.
func (k *Kernel) verifyOutput(ctx context.Context, goal, output string, complexity int) string {
	if k.verifier == nil || complexity < verifyComplexityMin {
		return output
	}
	k.log("verify", fmt.Sprintf("Post-completion verification (complexity %d)", complexity))
	vResult, vErr := k.verifier.QuickVerify(ctx, goal, output)
	if vErr != nil || vResult == nil {
		return output
	}
	k.log("verify", fmt.Sprintf("Verification confidence: %.2f (passed: %v)", vResult.Confidence.Overall, vResult.Passed))
	if k.emitter != nil {
		status := "passed"
		if !vResult.Passed {
			status = "failed"
		}
		k.emitter.EmitTaskUpdate("verification", status,
			fmt.Sprintf("Post-completion verification %s (confidence %.2f)", status, vResult.Confidence.Overall))
	}
	if !vResult.Passed && len(vResult.Issues) > 0 {
		output += "\n\n⚠ Verification found issues:\n- " + strings.Join(vResult.Issues, "\n- ")
	}
	return output
}

// degradedNoToolsPrompt is used ONLY when the tool-capable path could not run
// — the loop failed to reach a model, usually a transport error or an
// exhausted quota. It is a degraded answer, not a mode.
//
// It used to be the General-mode prompt, and it told the model to say the user
// should "switch to Project, Auto, or Loop mode". That advice was wrong twice
// over. General mode no longer exists — a conversational turn gets read-only
// tools — and when this prompt is reached the cause is a failed model call, so
// switching modes does nothing. Observed live: asked what was in a directory,
// the agent answered "I cannot see files. Please switch to a different mode."
// while holding a working list_files tool, because the loop had 429'd and this
// fallback spoke as though the tools were absent by design.
//
// So it now says what is actually true: the tools could not be reached this
// turn, and retrying is the useful advice.
const degradedNoToolsPrompt = "You are DarkCode. Your tools are temporarily unreachable for this turn — a " +
	"model or network call failed, which is usually a rate limit or an exhausted quota. Answer from what you " +
	"already know, and be explicit that you could not inspect the project this turn. Never claim you performed " +
	"an action, and never output a shell command as if it ran. If the answer needs the repository, the web, or a " +
	"file, say so and suggest retrying in a moment — switching modes will not help."

// executeDirectNoTools is the degraded fallback: a single LLM call with no
// tools, taken only when the tool-capable path could not run. Still
// participates in consensus when
// multiple models are registered, since that's a text-only refinement.

// chatContextRecentMax bounds how many recent messages are sent verbatim in
// Chat; older turns are covered by the rolling compressed summary instead.
const chatContextRecentMax = 8

// boundedChatContext returns a compact conversation for Chat: every rolling-
// summary briefing message (which carries older understanding) plus only the
// last chatContextRecentMax messages verbatim. Short conversations pass through
// unchanged.
func boundedChatContext(stm []core.Message) []core.Message {
	if len(stm) <= chatContextRecentMax {
		return stm
	}
	out := make([]core.Message, 0, chatContextRecentMax+2)
	for _, m := range stm[:len(stm)-chatContextRecentMax] {
		if s, ok := m.Content.(string); ok && m.Role == core.RoleSystem && strings.Contains(s, "[COMPRESSED CONTEXT]") {
			out = append(out, m) // keep the rolling summary of older turns
		}
	}
	out = append(out, stm[len(stm)-chatContextRecentMax:]...)
	return out
}

// executeChatReadOnly answers a Chat request using only read-only tools: the
// registry refuses mutating tools and the loop offers only read-only schemas,
// so Chat can never write. Reuses the ReAct loop, so a question needing no
// reads stays a single call. Falls back to plain text if the loop errors.
func (k *Kernel) executeChatReadOnly(ctx context.Context, goal string, recallBlock string) (string, error) {
	if k.agenticLoop == nil {
		return k.executeDirectNoTools(ctx, goal, recallBlock)
	}
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("chat", "planning", "Chat — reading to answer (read-only)")
	}
	roCtx := context.WithValue(ctx, core.ReadOnlyToolsKey, true)

	// Prior conversation excluding the current turn (STMAdd already appended
	// it), bounded to a compact summary + recent tail so a long chat doesn't
	// resend the whole transcript.
	stm := k.memory.STMGet()
	var history []core.Message
	if len(stm) > 0 {
		history = boundedChatContext(stm[:len(stm)-1])
	}

	loopRes, err := k.agenticLoop.Run(roCtx, k.injectRecall(goal, recallBlock), history)
	if err != nil {
		// Say so, loudly. This degrades to an answer with no tools, and a
		// degraded answer the user cannot tell apart from a normal one is
		// worse than an error: it looks like the agent chose not to look.
		k.log("chat", "read-only loop failed ("+err.Error()+") — answering without tools")
		if k.emitter != nil {
			k.emitter.EmitError("tools unreachable this turn (" + err.Error() + ") — answering from prior knowledge only")
		}
		return k.executeDirectNoTools(ctx, goal, recallBlock)
	}
	output := annotateUncited(loopRes.Output, recallBlock)
	k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
	// Chat is read-only, but a read-only loop can still abort or exhaust its
	// iterations, and that partial output must not enter memory as a
	// successful answer — see loop.Result.Completed.
	chatResult := &core.SubAgentResult{
		Role:      core.RoleWorker,
		Goal:      goal,
		Output:    output,
		Success:   loopRes.Completed,
		ToolCalls: loopRes.ToolCalls,
	}
	k.recordOutcome(goal, output, []*core.SubAgentResult{chatResult},
		loopRes.Completed, "chat", 0, recallBlock)
	if k.emitter != nil {
		k.emitter.EmitFinalOutput(output)
	}
	return output, nil
}

func (k *Kernel) executeDirectNoTools(ctx context.Context, goal string, recallBlock string) (string, error) {
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("general", "planning", "Conversational response (no tools)")
	}

	// Consensus path (text-only): if multiple models are registered and
	// consensus mode is on, fan out to both models and synthesize. This never
	// offers tools, so it cannot accidentally execute anything.
	if k.router.GetMode() == core.RouteConsensus && k.router.ModelCount() > 1 {
		k.log("consensus", "Running multi-model consensus (General mode, no tools)")
		output, err := k.runConsensus(ctx, goal, degradedNoToolsPrompt)
		if err == nil {
			k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
			k.recordOutcome(goal, output, nil, true, "general-consensus", 0, recallBlock)
			if k.emitter != nil {
				k.emitter.EmitFinalOutput(output)
			}
			return output, nil
		}
		k.log("consensus", "Consensus failed: "+err.Error()+" — falling back to single-model")
	}

	// Single-model path: route to the coding tier and call with NO tools.
	complexity := router.AssessComplexity(goal)
	client, modelName, err := k.router.Route(core.ModelTierCoding, complexity, goal)
	if err != nil {
		return "", fmt.Errorf("general mode: model routing failed: %w", err)
	}

	stm := k.memory.STMGet()

	sysContent := degradedNoToolsPrompt

	if recallBlock != "" {
		sysContent += "\n\n## Relevant Past Context\n" + recallBlock
	}

	// When UseCtxEngine is enabled, assemble a deduplicated, budget-trimmed
	// context window instead of dumping raw STM (Strategy 6b). Disabled by
	// default and falls back to the original raw-append behavior on any
	// error, so this is strictly opt-in.
	var messages []core.Message
	if engine := k.getCtxEngine(); engine != nil {
		window, err := engine.Assemble(ctx, ctxengine.AssembleRequest{
			Query:           goal,
			Conversation:    stm,
			SystemPrompt:    sysContent,
			AvailableTokens: client.ModelInfo().Context,
		})
		if err == nil && window != nil {
			messages = window.Messages
		}
	}
	if messages == nil {
		// Bound the conversation: rolling summary + recent tail, not the whole
		// transcript (Chat context economy).
		convo := boundedChatContext(stm)
		messages = make([]core.Message, 0, len(convo)+1)
		messages = append(messages, core.Message{Role: core.RoleSystem, Content: sysContent})
		messages = append(messages, convo...)
	}

	// Hard context-fit guarantee before dispatch (Part 3 contract): even when
	// the opt-in ctxengine didn't run, fit to the receiving client's effective
	// window so a long general-mode turn never overflows a local model.
	messages = compression.FitClient(messages, client, k.cfg.ContextLength, 0)

	temp := 0.7
	// Bound the reply. This answered every conversational turn with no
	// ceiling. The number comes from the one policy table.
	_, maxTok, _ := modelport.PolicyFor(modelport.PurposeConverse)
	req := &llm.CompletionRequest{
		Model:       modelName,
		Messages:    messages,
		Temperature: &temp,
		MaxTokens:   &maxTok,
		// Deliberately NO Tools field — General mode is tool-free.
	}

	resp, err := client.ChatCompletionStream(ctx, req, &llm.StreamCallbacks{
		OnContent: func(chunk string) {
			if k.emitter != nil {
				k.emitter.Emit(core.EventTaskUpdate, chunk,
					ui.WithTaskID("general"), ui.WithStatus("streaming"))
			}
		},
	})
	if err != nil {
		k.storeEpisodic(goal, "", nil, false, recallBlock, nil)
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("general mode: empty response")
	}
	output := resp.Choices[0].Message.Content

	k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: output})
	k.recordOutcome(goal, output, nil, true, "general", 0, recallBlock)
	if k.emitter != nil {
		k.emitter.EmitFinalOutput(output)
	}
	return output, nil
}

// semanticKey produces a stable, filesystem/JSON-safe key from a goal string.
func semanticKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	for _, ch := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r"} {
		s = strings.ReplaceAll(s, ch, "-")
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// extractPaths finds plausible file paths in the goal + output text.
func extractPaths(texts ...string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, text := range texts {
		for _, tok := range strings.Fields(text) {
			tok = strings.Trim(tok, "\"'`,.;()[]{}")
			if len(tok) < 3 || len(tok) > 256 {
				continue
			}
			// Absolute paths or paths with a slash and an extension.
			if (strings.HasPrefix(tok, "/") || strings.Contains(tok, "/")) && strings.Contains(tok, ".") {
				if !seen[tok] {
					seen[tok] = true
					paths = append(paths, tok)
				}
			}
		}
		if len(paths) >= 20 {
			break
		}
	}
	return paths
}

func generateSkillName(goal string) string {
	// Create a snake_case name from the goal. Strip punctuation by chaining
	// replacements on an accumulator — earlier code reassigned from the loop
	// variable `w` on each line, which discarded every replacement but the
	// last (commas and dots survived in skill names).
	words := strings.Fields(strings.ToLower(goal))
	if len(words) > 5 {
		words = words[:5]
	}
	for i, w := range words {
		clean := w
		for _, ch := range []string{",", ".", ":", ";", "!", "?"} {
			clean = strings.ReplaceAll(clean, ch, "")
		}
		words[i] = clean
	}
	return "skill_" + strings.Join(words, "_")
}

// pluralY returns "y"/"ies" for 1/non-1.
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
