package compression

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// Compressor is the Layer 3 compression agent. It uses a lightweight
// model call to reduce context between steps, extracting only meaningful
// signals and producing a structured ContextSnapshot.
//
// The compressor ALWAYS runs between major steps to keep token usage low
// and maintain stable memory growth over long sessions.
type Compressor struct {
	mu           sync.Mutex
	client       core.LLMClient
	fastModel    string // model name for compression (should be lightweight)
	enabled      bool
	lastSnapshot *core.ContextSnapshot
	useLocal     bool
	router       core.ModelRouter
}

// NewCompressor creates a compression agent.
func NewCompressor(client core.LLMClient, fastModel string, router core.ModelRouter) *Compressor {
	return &Compressor{
		client:    client,
		fastModel: fastModel,
		enabled:   true,
		router:    router,
		useLocal:  true, // Default to true to save API costs
	}
}

// SetUseLocal toggles whether to force the local model for compression.
func (c *Compressor) SetUseLocal(useLocal bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.useLocal = useLocal
}

// SetEnabled enables or disables compression.
func (c *Compressor) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
}

// SetClient hot-swaps the LLM client and model name used for compression. This
// is called by the kernel's ReloadModels so that a compressor-model change
// made via the GUI takes effect immediately, without restart.
func (c *Compressor) SetClient(client core.LLMClient, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = client
	c.fastModel = model
}

// FastClient returns the LLM client used for compression. The ctxengine uses
// this for LLM-backed summarization when assembling context windows.
func (c *Compressor) FastClient() core.LLMClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

// IsEnabled returns whether compression is active.
func (c *Compressor) IsEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// LastSnapshot returns the most recent compression result.
func (c *Compressor) LastSnapshot() *core.ContextSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSnapshot
}

// Compress takes a conversation history and produces a compressed
// ContextSnapshot. It sends the history to a fast model with a
// structured prompt, then parses the result.
func (c *Compressor) Compress(ctx context.Context, messages []core.Message, goal string) (*core.ContextSnapshot, error) {
	c.mu.Lock()
	enabled := c.enabled
	client := c.client
	fastModel := c.fastModel
	useLocal := c.useLocal
	router := c.router
	c.mu.Unlock()

	if !enabled || client == nil {
		// Fallback: simple heuristic compression (truncate to last N messages)
		return c.heuristicCompress(messages, goal), nil
	}

	if useLocal && router != nil {
		localClient, localModel, err := router.Route(core.ModelTierFast, 0, "local_compression")
		if err == nil && localClient != nil {
			client = localClient
			fastModel = localModel
			
			// Try to mount the summarizer LoRA
			if lm, ok := client.(core.LoRAManager); ok {
				_ = lm.MountLoRA("summarizer", 1.0)
				defer lm.MountLoRA("summarizer", 0.0)
			}
		}
	}

	// Estimate original token count (rough: ~4 chars per token)
	originalTokens := c.estimateTokens(messages)

	// Build compression prompt
	prompt := buildCompressionPrompt(messages, goal)

	temp := 0.1
	maxTok := 2000
	req := &core.CompletionRequest{
		Model: fastModel,
		Messages: []core.Message{
			{
				Role:    core.RoleSystem,
				Content: compressionSystemPrompt,
			},
			{
				Role:    core.RoleUser,
				Content: prompt,
			},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	resp, err := client.ChatCompletion(ctx, req)
	if err != nil {
		// Fallback to heuristic on error
		snapshot := c.heuristicCompress(messages, goal)
		snapshot.OriginalTokens = originalTokens
		return snapshot, nil
	}

	snapshot := parseCompressionResponse(resp.Choices[0].Message.Content, goal)
	snapshot.OriginalTokens = originalTokens
	snapshot.CompressedTokens = c.estimateSnapshotTokens(snapshot)
	snapshot.CompressedAt = time.Now()

	c.mu.Lock()
	c.lastSnapshot = snapshot
	c.mu.Unlock()

	return snapshot, nil
}

// heuristicCompress is the fallback when no LLM is available.
// It keeps only the most recent messages and extracts key information.
func (c *Compressor) heuristicCompress(messages []core.Message, goal string) *core.ContextSnapshot {
	snapshot := &core.ContextSnapshot{
		Goal:         goal,
		CompressedAt: time.Now(),
	}

	// Keep last few messages, extract user messages as decisions
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
				// First 200 chars as important context
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

	snapshot.OriginalTokens = c.estimateTokens(messages)
	snapshot.CompressedTokens = c.estimateSnapshotTokens(snapshot)
	return snapshot
}

// CompressBlock implements block-based incremental compression for the hierarchical
// context system. It compresses a specific block of messages, preserving high-importance
// ones based on heuristics, and caching the result to avoid drift.
func (c *Compressor) CompressBlock(ctx context.Context, messages []core.Message, goal string) (*core.CompressedBlock, error) {
	c.mu.Lock()
	enabled := c.enabled
	client := c.client
	fastModel := c.fastModel
	c.mu.Unlock()

	// 1. Identify important messages to pin
	_, pinnedIdxs := ScoreMessages(messages)
	var pinned []core.Message
	for _, idx := range pinnedIdxs {
		pinned = append(pinned, messages[idx])
	}

	// 2. Extract tool snapshots
	var toolSnaps []core.ToolSnapshot
	for _, msg := range messages {
		if msg.Role == core.RoleTool {
			snap := core.ToolSnapshot{
				Name:   msg.Name,
				Output: msg.ContentString(),
				Status: true,
			}
			if len(snap.Output) > 250 {
				snap.Output = snap.Output[:247] + "..."
			}
			toolSnaps = append(toolSnaps, snap)
		}
	}

	block := &core.CompressedBlock{
		BlockID:        fmt.Sprintf("block_%d", time.Now().UnixNano()),
		OriginalRange:  [2]int{0, len(messages)},
		PinnedMessages: pinned,
		ToolSnapshots:  toolSnaps,
		CompressedAt:   time.Now().Format(time.RFC3339),
	}

	if !enabled || client == nil {
		block.Summary = "(Compression disabled - raw messages omitted)"
		block.EstimatedTokens = EstimateTokens(pinned) + 50
		return block, nil
	}

	// Build summary prompt for the block
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation block concisely.\n\n")
	sb.WriteString(fmt.Sprintf("Current goal: %s\n\n", goal))
	for _, msg := range messages {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, msg.ContentString()))
	}

	temp := 0.2
	maxTok := 1000
	req := &core.CompletionRequest{
		Model: fastModel,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "You summarize conversation blocks concisely, extracting key facts and actions."},
			{Role: core.RoleUser, Content: sb.String()},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	resp, err := c.client.ChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("compress block: %w", err)
	} else {
		block.Summary = strings.TrimSpace(resp.Choices[0].Message.Content)
	}

	// Estimate final tokens (summary + pinned msgs)
	msgTokens := EstimateTokens(pinned)
	summaryTokens := EstimateStringTokens(block.Summary)
	block.EstimatedTokens = msgTokens + summaryTokens + 50 // some overhead

	return block, nil
}

// AssembleContext builds the final LLM context from STM messages within a token budget.
// It implements a weighted sliding window approach:
// 1. Keeps a recent sliding window of messages intact (up to ~40% of budget).
// 2. Pins high-importance older messages (tools, errors, key decisions) verbatim.
// 3. Compresses the remaining older, low-importance messages into a structured summary.
func (c *Compressor) AssembleContext(ctx context.Context, messages []core.Message, goal string, tokenBudget int) ([]core.Message, error) {
	// If it fits natively, just return it
	if FitsInBudget(messages, tokenBudget) {
		return messages, nil
	}

	// 1. Score messages for importance (heuristics from importance.go)
	_, pinnedIdxs := ScoreMessages(messages)
	
	// 2. Determine recent sliding window (up to 40% of budget)
	recentTokens := 0
	recentStart := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		toks := EstimateTokens([]core.Message{messages[i]})
		if recentTokens+toks > (tokenBudget * 40 / 100) {
			break
		}
		recentTokens += toks
		recentStart = i
	}
	// At least 4 messages if possible
	if len(messages)-recentStart < 4 && len(messages) > 4 {
		recentStart = len(messages) - 4
	}

	pinnedSet := make(map[int]bool)
	for _, idx := range pinnedIdxs {
		if idx < recentStart {
			pinnedSet[idx] = true
		}
	}

	var oldUnpinned []core.Message
	for i := 0; i < recentStart; i++ {
		if !pinnedSet[i] {
			oldUnpinned = append(oldUnpinned, messages[i])
		}
	}

	var result []core.Message

	// 3. Compress the old unpinned messages to reduce cloud burden
	if len(oldUnpinned) > 0 {
		snap, err := c.Compress(ctx, oldUnpinned, goal)
		if err == nil {
			result = append(result, SnapshotToMessages(snap)...)
		} else {
			// on failure, try to heuristic compress
			snap = c.heuristicCompress(oldUnpinned, goal)
			result = append(result, SnapshotToMessages(snap)...)
		}
	}

	// 4. Add pinned older messages verbatim (persisting key context)
	for i := 0; i < recentStart; i++ {
		if pinnedSet[i] {
			result = append(result, messages[i])
		}
	}

	// 5. Add the recent window
	result = append(result, messages[recentStart:]...)

	// Fallback if STILL over budget (due to too many pinned messages)
	if !FitsInBudget(result, tokenBudget) {
		snap, err := c.Compress(ctx, messages, goal)
		if err != nil {
			return messages, err
		}
		res := SnapshotToMessages(snap)
		hotCount := 4
		if len(messages) < hotCount {
			hotCount = len(messages)
		}
		res = append(res, messages[len(messages)-hotCount:]...)
		return res, nil
	}

	return result, nil
}

// estimateTokens gives a rough token count using the improved heuristic.
func (c *Compressor) estimateTokens(messages []core.Message) int {
	return EstimateTokens(messages)
}

// estimateSnapshotTokens estimates tokens in a compressed snapshot.
func (c *Compressor) estimateSnapshotTokens(snapshot *core.ContextSnapshot) int {
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

		// Parse "key: value" format
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
// single model selection in Settings governs ALL context compression. On error
// or when no client is configured, it falls back to a heuristic tail so the
// caller never blocks on compression failure.
//
// `focus` is a short label (e.g. the project name) injected into the prompt to
// keep the summary oriented around the right subject.
func (c *Compressor) Summarize(ctx context.Context, text, focus string) (string, error) {
	c.mu.Lock()
	enabled := c.enabled
	client := c.client
	fastModel := c.fastModel
	c.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}

	if !enabled || client == nil {
		return heuristicSummary(text), nil
	}

	focusLine := ""
	if strings.TrimSpace(focus) != "" {
		focusLine = fmt.Sprintf("Project: %s\n\n", focus)
	}

	temp := 0.2
	maxTok := 1500
	req := &core.CompletionRequest{
		Model: fastModel,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: summarySystemPrompt},
			{Role: core.RoleUser, Content: focusLine + "Summarize the following project context into a concise briefing:\n\n" + text},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	resp, err := c.client.ChatCompletion(ctx, req)
	if err != nil || len(resp.Choices) == 0 {
		// Fallback to a heuristic tail so a provider hiccup never breaks chat.
		return heuristicSummary(text), nil
	}
	out := strings.TrimSpace(resp.Choices[0].Message.Content)
	if out == "" {
		return heuristicSummary(text), nil
	}
	return out, nil
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

// SnapshotToMessages converts a ContextSnapshot back into a compact
// system message that can be injected into a new conversation.
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
