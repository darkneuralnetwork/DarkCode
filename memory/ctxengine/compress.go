package ctxengine

// compress.go holds Engine's LLM-backed compression methods: Compress
// (conversation history → structured ContextSnapshot, feeds STM compaction)
// and Summarize (arbitrary text → narrative briefing, feeds project
// context.md compaction). Both are a faithful port of kernel/compression's
// Compressor, moved here so they can finally dispatch through
// modelport.CompleteWith instead of calling client.ChatCompletion directly —
// kernel/compression couldn't do that itself because modelport imported it
// (for the window-fitting helpers, since relocated to infra/ctxfit).
//
// CompressBlock and AssembleContext were NOT ported: both were confirmed dead
// (no caller outside their own tests) during the investigation that produced
// this package's expansion — AssembleContext in particular duplicated what
// Engine.Assemble already does.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/ctxfit"
	"github.com/darkcode/infra/observability"
	"github.com/darkcode/kernel/modelport"
)

// loraLogf adapts the structured logger to the printf-style logger
// core.WithLoRA expects, so a missing/misplaced adapter surfaces in the log
// instead of silently degrading to the base model.
func loraLogf(format string, args ...interface{}) {
	observability.Log().Warn(fmt.Sprintf(format, args...), nil)
}

// SetClient hot-swaps the LLM client and model name used for compression.
// Called by the kernel's ReloadModels so a compressor-model change made via
// the GUI takes effect immediately, without restart.
//
// Also propagates to e.summarizer (IncrementalSummarizer.SetClient), so
// Assemble's own overflow-compression step (AdaptiveCompressor, which wraps
// this same summarizer instance) gets a real client too, instead of staying
// nil forever regardless of what this method sets — see SetClient's own
// comment for why splitting the two used to be the deliberate choice.
func (e *Engine) SetClient(client core.LLMClient, model string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.client = client
	e.model = model
	e.summarizer.SetClient(client)
}

// SetRouter configures the router Compress consults (when UseLocal is on) to
// prefer routing to a local model for compression, to save API cost.
func (e *Engine) SetRouter(router core.ModelRouter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.router = router
}

// SetUseLocal toggles whether Compress prefers routing to a local model
// (defaults to true, matching the old Compressor's default).
func (e *Engine) SetUseLocal(useLocal bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.useLocal = useLocal
}

// Compress takes a conversation history and produces a compressed
// ContextSnapshot. It sends the history to a fast model with a structured
// prompt, then parses the result. Falls back to a deterministic heuristic
// when no client is configured or the call fails.
//
// The old Compressor also had an enabled/disabled flag and a LastSnapshot
// getter; neither had a production caller (the flag was always left at its
// default), so both were dropped rather than carried forward as unused
// weight — matching Phase 1's rule for the ComputeTokenBudget family.
func (e *Engine) Compress(ctx context.Context, messages []core.Message, goal string) (*core.ContextSnapshot, error) {
	e.mu.Lock()
	client := e.client
	model := e.model
	useLocal := e.useLocal
	router := e.router
	e.mu.Unlock()

	if client == nil {
		return e.heuristicCompress(messages, goal), nil
	}

	useSummarizerLoRA := false
	if useLocal && router != nil {
		localClient, localModel, err := router.Route(core.ModelTierFast, 0, "local_compression")
		if err == nil && localClient != nil {
			client = localClient
			model = localModel
			useSummarizerLoRA = true
		}
	}

	originalTokens := ctxfit.EstimateTokens(messages)
	prompt := buildCompressionPrompt(messages, goal)

	ask := modelport.Ask{
		Purpose: modelport.PurposeCompress,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: compressionSystemPrompt},
			{Role: core.RoleUser, Content: prompt},
		},
		MaxTokens: 2000,
	}

	// Mount the summarizer LoRA for the local compression call via the single
	// audited path (logs a mount failure instead of silently using the base
	// model). A no-op for cloud/non-LoRA clients.
	var ans *modelport.Answer
	var err error
	call := func() error {
		ans, err = ctxengineModelManager.CompleteWith(ctx, client, model, ask)
		return err
	}
	if useSummarizerLoRA {
		_ = core.WithLoRA(client, "local_compression", loraLogf, call)
	} else {
		_ = call()
	}
	if err != nil || ans == nil {
		snapshot := e.heuristicCompress(messages, goal)
		snapshot.OriginalTokens = originalTokens
		return snapshot, nil
	}

	snapshot := parseCompressionResponse(ans.Text, goal)
	snapshot.OriginalTokens = originalTokens
	snapshot.CompressedTokens = estimateSnapshotTokens(snapshot)
	snapshot.CompressedAt = time.Now()

	return snapshot, nil
}

// heuristicCompress is the fallback when no LLM is available. It keeps only
// the most recent messages and extracts key information.
func (e *Engine) heuristicCompress(messages []core.Message, goal string) *core.ContextSnapshot {
	snapshot := &core.ContextSnapshot{
		Goal:         goal,
		CompressedAt: time.Now(),
	}

	recent := messages
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}

	for _, msg := range recent {
		switch msg.Role {
		case core.RoleUser:
			if msg.ContentString() != "" {
				snapshot.ActiveTasks = append(snapshot.ActiveTasks, msg.ContentString())
			}
		case core.RoleAssistant:
			content := msg.ContentString()
			if content != "" {
				if len(content) > 200 {
					content = content[:200] + "..."
				}
				snapshot.ImportantContext = append(snapshot.ImportantContext, content)
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					snapshot.NextActions = append(snapshot.NextActions,
						fmt.Sprintf("tool:%s", tc.Function.Name))
				}
			}
		}
	}

	snapshot.OriginalTokens = ctxfit.EstimateTokens(messages)
	snapshot.CompressedTokens = estimateSnapshotTokens(snapshot)
	return snapshot
}

// estimateSnapshotTokens estimates tokens in a compressed snapshot.
func estimateSnapshotTokens(snapshot *core.ContextSnapshot) int {
	total := len(snapshot.Goal) / 4
	for _, s := range snapshot.ActiveTasks {
		total += len(s) / 4
	}
	for _, s := range snapshot.Constraints {
		total += len(s) / 4
	}
	for _, s := range snapshot.Decisions {
		total += len(s) / 4
	}
	for _, s := range snapshot.Errors {
		total += len(s) / 4
	}
	for _, s := range snapshot.ImportantContext {
		total += len(s) / 4
	}
	for _, s := range snapshot.NextActions {
		total += len(s) / 4
	}
	return total
}

const compressionSystemPrompt = `You are a Compression Agent. Your job is to compress conversation history into a structured summary that preserves only meaningful signals.

You MUST output in this exact format:

goal: <the user's primary objective>
active_tasks: <comma-separated list of tasks currently in progress>
constraints: <comma-separated list of constraints or requirements>
decisions: <comma-separated list of decisions made so far>
errors: <comma-separated list of errors encountered>
important_context: <comma-separated list of critical context that must not be lost>
next_actions: <comma-separated list of recommended next steps>

Rules:
- Be extremely concise
- Remove all redundancy
- Extract only actionable information
- Preserve any file paths, command names, error messages verbatim
- Do not include conversational filler
- Do not include tool output details unless they contain errors or key results`

func buildCompressionPrompt(messages []core.Message, goal string) string {
	var sb strings.Builder
	sb.WriteString("Compress the following conversation history.\n\n")
	sb.WriteString(fmt.Sprintf("Current goal: %s\n\n", goal))
	sb.WriteString("Conversation history:\n\n")

	for _, msg := range messages {
		role := string(msg.Role)
		content := msg.ContentString()
		if content == "" && len(msg.ToolCalls) > 0 {
			content = "[tool calls: "
			for i, tc := range msg.ToolCalls {
				if i > 0 {
					content += ", "
				}
				content += tc.Function.Name
			}
			content += "]"
		}
		if content != "" {
			sb.WriteString(fmt.Sprintf("[%s]: %s\n", role, content))
		}
	}

	sb.WriteString("\n\nOutput the compressed summary in the required format.")
	return sb.String()
}

// parseCompressionResponse parses the structured output from the compression model.
func parseCompressionResponse(text, goal string) *core.ContextSnapshot {
	snapshot := &core.ContextSnapshot{
		Goal: goal,
	}

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])

		items := splitCSV(value)

		switch key {
		case "goal":
			if value != "" {
				snapshot.Goal = value
			}
		case "active_tasks":
			snapshot.ActiveTasks = items
		case "constraints":
			snapshot.Constraints = items
		case "decisions":
			snapshot.Decisions = items
		case "errors":
			snapshot.Errors = items
		case "important_context":
			snapshot.ImportantContext = items
		case "next_actions":
			snapshot.NextActions = items
		}
	}

	return snapshot
}

// splitCSV splits a comma-separated value list, trimming whitespace.
func splitCSV(s string) []string {
	if s == "" || s == "none" || s == "N/A" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// Summarize produces a concise narrative markdown briefing of an arbitrary
// body of text (used for persistent project-context compression). It uses the
// SAME client + model as Compress (the configured compressor model), so a
// single model selection in Settings governs ALL context compression. On
// error or when no client is configured, it falls back to a heuristic tail so
// the caller never blocks on compression failure.
//
// `focus` is a short label (e.g. the project name) injected into the prompt
// to keep the summary oriented around the right subject.
func (e *Engine) Summarize(ctx context.Context, text, focus string) (string, error) {
	e.mu.Lock()
	client := e.client
	model := e.model
	e.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}

	if client == nil {
		return heuristicSummary(text), nil
	}

	focusLine := ""
	if strings.TrimSpace(focus) != "" {
		focusLine = fmt.Sprintf("Project: %s\n\n", focus)
	}

	temp := 0.2
	ask := modelport.Ask{
		Purpose: modelport.PurposeCompress,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: summarySystemPrompt},
			{Role: core.RoleUser, Content: focusLine + "Summarize the following project context into a concise briefing:\n\n" + text},
		},
		MaxTokens:   1500,
		Temperature: &temp,
	}

	ans, err := ctxengineModelManager.CompleteWith(ctx, client, model, ask)
	if err != nil || ans == nil || strings.TrimSpace(ans.Text) == "" {
		// Fallback to a heuristic tail so a provider hiccup never breaks chat.
		return heuristicSummary(text), nil
	}
	return strings.TrimSpace(ans.Text), nil
}

// heuristicSummary is the no-LLM fallback for Summarize: it keeps the most
// recent portion of the context (the part most likely to be relevant) and
// trims older history. This guarantees a usable briefing even when the
// compressor model is unavailable.
func heuristicSummary(text string) string {
	const tail = 8 * 1024 // 8 KiB tail
	if len(text) <= tail {
		return text
	}
	return "…(older context trimmed)…\n\n" + text[len(text)-tail:]
}

const summarySystemPrompt = `You are a Compression Agent. Summarize the provided project context into a concise, information-dense briefing that an AI agent can read BEFORE starting work on the project.

Preserve, in concise markdown:
- the project's goal and current state
- key decisions and conventions adopted
- file/module structure and important paths
- open tasks and next steps
- any errors or blockers still active

Rules:
- Be concise but preserve all verbatim file paths, command names, and error messages.
- Drop conversational filler and redundant Q/A logs.
- Do NOT invent facts not present in the source text.
- Output only the markdown briefing.`

// SnapshotToMessages converts a ContextSnapshot back into a compact system
// message that can be injected into a new conversation.
func SnapshotToMessages(snapshot *core.ContextSnapshot) []core.Message {
	var sb strings.Builder
	sb.WriteString("[COMPRESSED CONTEXT]\n")
	sb.WriteString(fmt.Sprintf("goal: %s\n", snapshot.Goal))

	if len(snapshot.ActiveTasks) > 0 {
		sb.WriteString(fmt.Sprintf("active_tasks: %s\n", strings.Join(snapshot.ActiveTasks, "; ")))
	}
	if len(snapshot.Constraints) > 0 {
		sb.WriteString(fmt.Sprintf("constraints: %s\n", strings.Join(snapshot.Constraints, "; ")))
	}
	if len(snapshot.Decisions) > 0 {
		sb.WriteString(fmt.Sprintf("decisions: %s\n", strings.Join(snapshot.Decisions, "; ")))
	}
	if len(snapshot.Errors) > 0 {
		sb.WriteString(fmt.Sprintf("errors: %s\n", strings.Join(snapshot.Errors, "; ")))
	}
	if len(snapshot.ImportantContext) > 0 {
		sb.WriteString(fmt.Sprintf("important_context: %s\n", strings.Join(snapshot.ImportantContext, "; ")))
	}
	if len(snapshot.NextActions) > 0 {
		sb.WriteString(fmt.Sprintf("next_actions: %s\n", strings.Join(snapshot.NextActions, "; ")))
	}
	sb.WriteString("[/COMPRESSED CONTEXT]")

	return []core.Message{
		{
			Role:    core.RoleSystem,
			Content: sb.String(),
		},
	}
}

// SnapshotToMessages forwards to the package function of the same name, so a
// caller holding the engine through the orchestrator's contextCompressor
// interface (which avoids importing this package for a concrete type) can
// reach it without naming the package.
func (e *Engine) SnapshotToMessages(snapshot *core.ContextSnapshot) []core.Message {
	return SnapshotToMessages(snapshot)
}
