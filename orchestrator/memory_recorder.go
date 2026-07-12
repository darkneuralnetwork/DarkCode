package orchestrator

import (
	"fmt"
	"github.com/darkcode/internal/strutil"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
)

// storeEpisodic archives the task details in the episodic memory tier.
func (k *Kernel) storeEpisodic(goal string, output string, agentResults []*core.SubAgentResult, success bool, injectedRecall string) {
	if k.memory == nil {
		return
	}

	var totalDuration time.Duration
	var toolsUsed []string
	var steps []string

	for _, r := range agentResults {
		d, _ := time.ParseDuration(r.Duration)
		totalDuration += d
		steps = append(steps, r.Goal)
		for _, tc := range r.ToolCalls {
			toolsUsed = append(toolsUsed, tc.Function.Name)
		}
	}

	summary := goal
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	outcome := "success"
	if !success {
		outcome = "failure"
	}

	entry := core.EpisodicEntry{
		TaskGoal:       goal,
		Outcome:        outcome,
		Summary:        summary,
		Output:         output, // full output for exact-match cache (General mode)
		Steps:          steps,
		Duration:       totalDuration.String(),
		ToolsUsed:      toolsUsed,
		InjectedRecall: injectedRecall,
		Timestamp:      time.Now(),
	}

	_ = k.memory.EpisodicAdd(entry)
}

// recordLearningAndAudit records learning feedback + an audit-trail entry
// after a task completes. Called from every execution path (loop/DAG/direct/
// general[-consensus]/clarification) so the Learning Engine and Audit tabs
// are populated regardless of which path the kernel took.
//
// When minSkillSuccess > 0, it also attempts procedural-skill extraction
// (see extractSkill) using the SAME success/tool-usage counts computed here,
// instead of the caller separately re-deriving them from results a second
// time. Previously LearningEngine's task-type strategies and
// skill_extractor's per-goal skills were two independent call sites that
// each iterated `results` on their own — same data, two passes, no shared
// signal. Pass minSkillSuccess=0 (the common case) to skip skill extraction
// entirely, matching the callers that never extracted a skill before this
// consolidation (loop, general, general-consensus, clarification, DAG
// failure).
func (k *Kernel) recordLearningAndAudit(goal, output string, results []*core.SubAgentResult, success bool, strategy string, minSkillSuccess int) {
	var toolsUsed []string
	var agentsUsed []core.AgentRole
	successCount := 0
	for _, r := range results {
		agentsUsed = append(agentsUsed, r.Role)
		if r.Success {
			successCount++
		}
		for _, tc := range r.ToolCalls {
			toolsUsed = append(toolsUsed, tc.Function.Name)
		}
	}

	if k.memory.Learning() != nil {
		_ = k.memory.Learning().RecordFeedback(core.LearningFeedback{
			TaskGoal:   goal,
			Success:    success,
			ToolsUsed:  toolsUsed,
			AgentsUsed: agentsUsed,
			Strategy:   strategy,
		})
	}

	if k.memory.Audit() != nil {
		outcome := "success"
		risk := core.RiskLow
		if !success {
			outcome = "failure"
			risk = core.RiskMedium
		}
		toolSummary := strings.Join(toolsUsed, ",")
		_ = k.memory.Audit().RecordAction(
			core.RoleExecutive, strategy, toolSummary,
			risk, true, outcome,
		)
	}

	// Populate the knowledge graph with entities extracted from this task.
	k.populateKnowledgeGraph(goal, output, toolsUsed, agentsUsed, success)

	// Store key facts in semantic memory so the Semantic tab is populated
	// and the agent can recall what it previously did to similar requests.
	k.storeSemanticFacts(goal, output, toolsUsed, success)

	if minSkillSuccess > 0 {
		k.extractSkill(goal, results, successCount, toolsUsed, minSkillSuccess)
	}
}

// injectRecall prepends a compact "Relevant Past Context" block to the goal
// when the hybrid retriever finds related past tasks/knowledge. This is the
// lightweight RAG half of the hybrid KG+retrieval architecture: it gives the
// planner the benefit of prior executions (e.g. recalling a past "calculator"
// task when asked to "build an arithmetic tool") without any embedding-model
// getRecallBlock returns a compact "Relevant Past Context" block for the goal
// or an empty string if there are no hits or no retriever.
func (k *Kernel) getRecallBlock(goal string) string {
	if k.retriever == nil {
		return ""
	}
	hits := k.retriever.Recall(goal, 3)
	block := memory.FormatRecall(hits)
	if block == "" {
		return ""
	}
	k.log("memory", fmt.Sprintf("Hybrid recall: %d relevant past entr%s injected", len(hits), pluralY(len(hits))))
	return block
}

// injectRecall prepends a compact "Relevant Past Context" block to the goal
// returning the modified goal string.
func (k *Kernel) injectRecall(goal string, block string) string {
	if block == "" {
		return goal
	}
	return block + "\n## Task\n" + goal
}

// pluralY returns "y"/"ies" for 1/non-1.
func (k *Kernel) populateKnowledgeGraph(goal, output string, toolsUsed []string, agentsUsed []core.AgentRole, success bool) {
	kg := k.memory.KG()
	if kg == nil {
		return
	}
	now := time.Now()

	// Task node.
	taskID := "task:" + strutil.TruncateID(goal, 60)
	outcome := "success"
	if !success {
		outcome = "failure"
	}
	_ = kg.AddNode(&core.KGNode{
		ID:         taskID,
		Label:      strutil.Truncate(goal, 80),
		Type:       core.KGNodeTask,
		Properties: map[string]string{"outcome": outcome, "strategy": "kernel"},
		CreatedAt:  now,
	})

	// File nodes — extract paths mentioned in the goal/output.
	for _, p := range extractPaths(goal, output) {
		fileID := "file:" + p
		_ = kg.AddNode(&core.KGNode{
			ID:        fileID,
			Label:     p,
			Type:      core.KGNodeFile,
			CreatedAt: now,
		})
		_ = kg.Relate(taskID, fileID, core.KGRelContains)
	}

	// Tool nodes.
	seen := make(map[string]bool)
	for _, t := range toolsUsed {
		if seen[t] {
			continue
		}
		seen[t] = true
		toolID := "tool:" + t
		_ = kg.AddNode(&core.KGNode{
			ID:        toolID,
			Label:     t,
			Type:      core.KGNodeTool,
			CreatedAt: now,
		})
		_ = kg.Relate(taskID, toolID, core.KGRelUsedBy)
	}

	// Agent nodes.
	seenA := make(map[string]bool)
	for _, a := range agentsUsed {
		name := string(a)
		if seenA[name] {
			continue
		}
		seenA[name] = true
		agentID := "agent:" + name
		_ = kg.AddNode(&core.KGNode{
			ID:        agentID,
			Label:     name,
			Type:      core.KGNodeAgent,
			CreatedAt: now,
		})
		_ = kg.Relate(taskID, agentID, core.KGRelProducedBy)
	}

	// Word relations — extract concept co-occurrences from the goal + output so
	// the KG captures which ideas the agent reasoned about together. This is
	// the "word relations" layer: concept nodes (type=concept) linked by
	// related_to edges weighted by co-occurrence. Batched (single save).
	_ = kg.RecordWordRelations(goal + "\n" + output)
}

// storeSemanticFacts records a concise knowledge entry in semantic memory
// for each completed task, so the Semantic tab reflects real activity and
// the agent can recall prior outcomes. The key is derived from the goal so
// re-running a similar task updates (rather than duplicates) the entry.
func (k *Kernel) storeSemanticFacts(goal, output string, toolsUsed []string, success bool) {
	if k.memory == nil {
		return
	}
	key := "task:" + semanticKey(goal)

	outcome := "success"
	if !success {
		outcome = "failure"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Goal: %s\n", goal))
	sb.WriteString(fmt.Sprintf("Outcome: %s\n", outcome))
	if len(toolsUsed) > 0 {
		// Dedupe tools while preserving order.
		seen := make(map[string]bool)
		var uniq []string
		for _, t := range toolsUsed {
			if !seen[t] {
				seen[t] = true
				uniq = append(uniq, t)
			}
		}
		sb.WriteString("Tools: " + strings.Join(uniq, ", ") + "\n")
	}
	if paths := extractPaths(goal, output); len(paths) > 0 {
		sb.WriteString("Files: " + strings.Join(paths, ", ") + "\n")
	}
	summary := strutil.Truncate(output, 240)
	if summary != "" {
		sb.WriteString("Result: " + summary)
	}

	_ = k.memory.SemanticAdd(key, sb.String(), "task", []string{outcome, "task"})
}

// semanticKey produces a stable, filesystem/JSON-safe key from a goal string.
