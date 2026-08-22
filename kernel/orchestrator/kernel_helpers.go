package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/agents"
	"github.com/darkcode/kernel/modelport"
	"github.com/darkcode/kernel/router"
	"github.com/darkcode/memory/ctxengine"
	"github.com/darkcode/surfaces/ui"
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
	// verifies its output when the task is meaty enough to warrant it, OR
	// when it wrote to the workspace at all. The complexity gate alone let a
	// short-looking task ("add a function, add a test, build and test") skip
	// verification entirely while still claiming success — a broken build
	// went unnoticed because nothing checked it. A task that never touched
	// the filesystem has nothing for gofmt/build/test to catch, so read-only
	// answers keep the cheap complexity-only path.
	// The pipeline's stages are deterministic (gofmt/build/tests/style, each
	// gated by IsApplicable) so this costs no LLM calls; failures are
	// surfaced in the answer instead of silently logged.
	output := result.Output
	complexity := router.AssessComplexity(goal)
	if k.usedMutatingTool(result.ToolCalls) {
		complexity = verifyComplexityMin
	}
	output = k.verifyOutput(ctx, goal, output, complexity)

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

// usedMutatingTool reports whether any of the given calls invoked a tool that
// can change the workspace (write_file, patch, terminal, ...) rather than
// only observing it. Unknown tool names are treated as mutating — the safe
// default when a name can't be looked up is to verify, not to skip.
func (k *Kernel) usedMutatingTool(calls []core.ToolCall) bool {
	if k.registry == nil {
		return false
	}
	for _, c := range calls {
		entry, ok := k.registry.Get(c.Function.Name)
		if !ok || !entry.ReadOnly {
			return true
		}
	}
	return false
}

// VerificationIssuesMarker prefixes the block verifyOutput appends to an
// answer when post-completion verification fails. Surfaces that report a
// separate machine-readable success flag (surfaces/server/chat_handler.go's
// JSON response, notably) check for this marker rather than hardcoding
// success — the alternative was a response that says "success": true over an
// answer whose own text says the build is broken.
const VerificationIssuesMarker = "⚠ Verification found issues:"

// verifyOutput runs the self-verification pipeline when the task complexity
// warrants it, emits the outcome, and appends any found issues to the output
// so the user sees verification results instead of a silent log line.
func (k *Kernel) verifyOutput(ctx context.Context, goal, output string, complexity int) string {
	if k.verifier == nil || complexity < verifyComplexityMin {
		return output
	}
	k.log("verify", fmt.Sprintf("Post-completion verification (complexity %d)", complexity))
	// k.verifier is built once at kernel construction with an empty
	// workspace, which made every command-based stage check whatever
	// directory the process happened to be launched from instead of the
	// active per-request workspace — silently verifying the wrong project
	// once the active workspace ever differs from that (the normal case for
	// a long-running server). loop.go's agentic-loop verifier already builds
	// itself fresh per call with the real workspace; this mirrors that.
	verifier := agents.NewVerificationPipeline(k.router, k.emitter, core.WorkspaceFrom(ctx))
	vResult, vErr := verifier.QuickVerify(ctx, goal, output)
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
		output += "\n\n" + VerificationIssuesMarker + "\n- " + strings.Join(vResult.Issues, "\n- ")
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

// chatContextRecentMax bounds how many recent messages are sent verbatim
// when cfg.UseCtxEngine is off; older turns are covered by the rolling
// compressed summary instead. Only used by boundedChatContext.
const chatContextRecentMax = 8

// boundedChatContext is the pre-ctxengine fallback for bounding Chat/General
// history: every rolling-summary briefing message (which carries older
// understanding) plus only the last chatContextRecentMax messages verbatim.
// Short conversations pass through unchanged.
//
// Used ONLY when cfg.UseCtxEngine is off (executeChatReadOnly,
// executeDirectNoTools) — when it's on, ctxengine.Engine.Assemble replaces
// this and does strictly better (keeps EVERY system message, not just ones
// found in the trimmed-off older segment, and ranks/trims the rest instead of
// a flat recent-N cutoff). This still needs to exist for the off case: it was
// previously called unconditionally, and briefly wasn't called at all here
// during Phase 3 of the context-management unification — plain raw STM
// relies on ctxfit.FitToWindow's oldest-first shedding as its only bound,
// which anchors just the LEADING system message, not a rolling summary
// sitting further back in history, so a long off-flag Chat could silently
// lose its compressed-context briefing. Restored once that gap was caught in
// review.
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
	// it). When cfg.UseCtxEngine is on, passed to the loop raw — Run assembles
	// it (dedup, rank, budget-trim, restore chronological order) via its own
	// ctxengine.Engine, which the kernel shares with the loop
	// (SetContextEngine + SetUseContextEngine in New); bounding it again here
	// first would just assemble twice, and the loop is the single choke
	// point. When it's off, bound it here the same way General mode does
	// (boundedChatContext) — Run's own fallback for that case is a raw,
	// unranked, untrimmed append, which is fine size-wise (ctxfit.FitClient
	// still bounds it at the wire) but doesn't know to protect a rolling
	// compressed-context summary sitting mid-history the way
	// boundedChatContext does.
	stm := k.memory.STMGet()
	var history []core.Message
	if len(stm) > 0 {
		history = stm[:len(stm)-1]
		if !k.cfg.UseCtxEngine {
			history = boundedChatContext(history)
		}
	}

	// k.injectRecall(goal, recallBlock) is both the actual goal message AND
	// (when useContextEngine is on) the Query Assemble ranks history against —
	// so relevance ranking currently scores against goal+recall-block text,
	// not the bare user ask, which dilutes which history survives trimming.
	// Harmless today (recallBlock is capped, budget is generous) but real;
	// stops being implicit once injections carry their own AssembleRequest
	// field instead of being pre-concatenated into the goal — Phase 4 of the
	// context-management unification.
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
	client, _, err := k.router.Route(core.ModelTierCoding, complexity, goal)
	if err != nil {
		return "", fmt.Errorf("general mode: model routing failed: %w", err)
	}

	stm := k.memory.STMGet()

	sysContent := degradedNoToolsPrompt

	// When UseCtxEngine is enabled, assemble a deduplicated, relevance-ranked,
	// budget-trimmed context window (memory/ctxengine's Engine.Assemble)
	// instead of dumping raw STM. Disabled by default (Phase 5 of the context-
	// management unification flips this) and falls back to a raw append on
	// any error, so this is strictly opt-in until then — the dispatch-time
	// ctxfit.FitClient backstop inside k.models.Complete still bounds the raw
	// fallback before it reaches the wire either way.
	//
	// The recall block goes in as an Injection here, not concatenated into
	// sysContent (its pre-unification treatment): Assemble ranks/budgets it
	// alongside the conversation instead of it unconditionally winning space
	// as if it were part of the system prompt — see AssembleRequest.Injections.
	var messages []core.Message
	if k.cfg.UseCtxEngine && k.ctxEngine != nil {
		var injections []core.Message
		if recallBlock != "" {
			injections = []core.Message{{Role: core.RoleSystem, Content: "## Relevant Past Context\n" + recallBlock}}
		}
		window, err := k.ctxEngine.Assemble(ctx, ctxengine.AssembleRequest{
			Query:           goal,
			Conversation:    stm,
			SystemPrompt:    sysContent,
			Injections:      injections,
			AvailableTokens: client.ModelInfo().Context,
		})
		if err == nil && window != nil {
			messages = window.Messages
		}
	}
	if messages == nil {
		// Off, or Assemble errored: bound the conversation the same way Chat
		// does (boundedChatContext) rather than appending raw STM — see its
		// comment for why a flat append isn't equivalent once history is long
		// enough for FitToWindow's oldest-first shedding to reach a rolling
		// compressed-context summary sitting mid-history. The recall block
		// folds into the system content directly here (the pre-unification
		// behavior) since this fallback doesn't rank/budget it separately.
		fallbackSys := sysContent
		if recallBlock != "" {
			fallbackSys += "\n\n## Relevant Past Context\n" + recallBlock
		}
		convo := boundedChatContext(stm)
		messages = make([]core.Message, 0, len(convo)+1)
		messages = append(messages, core.Message{Role: core.RoleSystem, Content: fallbackSys})
		messages = append(messages, convo...)
	}

	// One call through the model manager. It picks the tier for the purpose,
	// applies the ceiling and temperature from the one policy table, and fits
	// the prompt to the window of the model it actually chose — which is why
	// the explicit FitClient that used to sit here is gone. Deliberately no
	// Tools: this path answers without acting.
	ans, err := k.models.Complete(ctx, modelport.Ask{
		Purpose:  modelport.PurposeConverse,
		Messages: messages,
		Goal:     goal,
		Stream: &core.StreamCallbacks{
			OnContent: func(chunk string) {
				if k.emitter != nil {
					k.emitter.Emit(core.EventTaskUpdate, chunk,
						ui.WithTaskID("general"), ui.WithStatus("streaming"))
				}
			},
		},
	})
	if err != nil {
		k.storeEpisodic(goal, "", nil, false, recallBlock, nil)
		return "", err
	}
	output := ans.Text

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
