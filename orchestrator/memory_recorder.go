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
// lessons (may be nil) populates LessonsLearned — pass nil for the hard-error
// paths that never reach recordOutcome (a true err != nil has no reflection
// worth computing; see kernel.go's early-return storeEpisodic calls).
func (k *Kernel) storeEpisodic(goal string, output string, agentResults []*core.SubAgentResult, success bool, injectedRecall string, lessons []string) {
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
		LessonsLearned: lessons,
		InjectedRecall: injectedRecall,
		Timestamp:      time.Now(),
	}

	_ = k.memory.EpisodicAdd(entry)
}

// recordOutcome is the single post-task promotion path (local-first upgrade
// §7 Phase D). It computes toolsUsed/agentsUsed/successCount from `results`
// ONCE, reflects on why the task succeeded/failed (see reflection.go — no
// LLM call), and fans that out to every memory tier: episodic (with the
// reflection's lessons attached), the learning engine (Lessons populated the
// same way), the audit trail, the knowledge graph (including typed fix/
// decision facts when the reflection identifies one), semantic memory, and
// optional procedural-skill extraction.
//
// Called from every execution path (loop/DAG/direct/general[-consensus]/
// clarification) so the Learning Engine and Audit tabs are populated
// regardless of which path the kernel took. This replaces the previous
// storeEpisodic + recordLearningAndAudit pair, which were always called
// together with identical arguments yet independently derived toolsUsed from
// `results` — two passes over the same data with no shared signal, and
// neither populated LessonsLearned/Lessons at all.
//
// When minSkillSuccess > 0, it also attempts procedural-skill extraction
// (see extractSkill) using the SAME success/tool-usage counts computed here.
// Pass minSkillSuccess=0 (the common case) to skip skill extraction entirely,
// matching the callers that never extracted a skill before this
// consolidation (loop, general, general-consensus, clarification, DAG
// failure).
func (k *Kernel) recordOutcome(goal, output string, results []*core.SubAgentResult, success bool, strategy string, minSkillSuccess int, recallBlock string) {
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

	reflection := k.reflect(goal, output, toolsUsed, success, strategy)

	k.storeEpisodic(goal, output, results, success, recallBlock, reflection.Lessons)

	if k.memory.Learning() != nil {
		_ = k.memory.Learning().RecordFeedback(core.LearningFeedback{
			TaskGoal:   goal,
			Success:    success,
			ToolsUsed:  toolsUsed,
			AgentsUsed: agentsUsed,
			Strategy:   strategy,
			Lessons:    reflection.Lessons,
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

	// Populate the knowledge graph with entities extracted from this task,
	// including typed fix/decision facts when the reflection found one.
	k.populateKnowledgeGraph(goal, output, toolsUsed, agentsUsed, success, reflection)

	// Store key facts in semantic memory — but only for outcomes worth keeping.
	// A trivial conversational Q&A (no tools, no code, not a fix/decision) is
	// not durable knowledge; persisting every one of them pollutes the semantic
	// tier and the RAG context it feeds. Episodic memory still records every
	// turn (for the exact-repeat answer cache), so skipping this write does not
	// hurt instant recall of repeats.
	if worthPersistingSemantic(output, toolsUsed, reflection) {
		k.storeSemanticFacts(goal, output, toolsUsed, success)
	}

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
	// Fetch a wider set, then drop episodic (conversation) hits from before the
	// current session so a "New Chat" doesn't resurface prior conversations.
	// Durable semantic/KG facts are session-independent and always kept. We
	// over-fetch (10) before the epoch filter so trimming stale episodics can't
	// starve out valid semantic hits that ranked just below them.
	hits := k.retriever.Recall(goal, 10)
	if epoch := k.memory.SessionEpoch(); !epoch.IsZero() {
		filtered := hits[:0]
		for _, h := range hits {
			// Suppress conversational outcomes from a previous session: both
			// episodic entries AND the "task:"-keyed semantic facts that
			// storeSemanticFacts writes for every Q&A (these are prior chats,
			// not durable knowledge). Genuine non-task semantic entries are
			// session-independent and always kept.
			conversational := h.Source == "episodic" || strings.HasPrefix(h.ID, "task:")
			if conversational && h.Timestamp.Before(epoch) {
				continue
			}
			filtered = append(filtered, h)
		}
		hits = filtered
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
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

// decisionFactConfidence is the write-time confidence for a decision fact.
// Flat (unlike fixFactConfidence): a decision has no analogous "prior
// failure" evidence to grade against — it's promoted from keyword
// classification alone, so there's nothing graduated to weigh. Still above
// the cascade's default 0.75 answer threshold (orchestrator/cascade.go's
// cascadeDefaultThreshold) so a recorded decision can answer on its own.
const decisionFactConfidence = 0.78

// fixFactConfidenceBase/Max bound the graduated confidence a fix fact can
// receive from corroborating evidence (see fixFactConfidence). The floor
// always clears the cascade's default 0.75 answer threshold — a single
// proven fix should be enough to answer a later similar question without an
// LLM call (plan §7 Phase D's stated effect) — while the ceiling stays
// below the 0.85+ reserved for AST-verified code facts (kg_answer.go's
// answerDefinition/answerImporters), since this is inferred from episodic
// outcome correlation, not verified causality.
const (
	fixFactConfidenceBase = 0.75
	fixFactConfidenceMax  = 0.84
)

// fixFactConfidence grades a fix fact's write-time confidence from the
// reflection's corroborating evidence instead of a flat pass/fail value:
// stronger wording similarity, overlapping file paths, and a fast
// failure→fix turnaround each add a small amount of trust. A fix fact
// promoted from keyword classification alone (no matched prior failure —
// reflection.ProblemGoal == "") gets the base confidence with no bonuses,
// since there's no lookback evidence to grade.
func fixFactConfidence(r Reflection) float64 {
	conf := fixFactConfidenceBase
	if r.ProblemGoal == "" {
		return conf
	}
	if r.Similarity >= 0.85 {
		conf += 0.03 // near-identical wording, not just loosely similar
	}
	if r.FileOverlap {
		conf += 0.04 // the fix touched the same file(s) as the problem
	}
	if r.MatchAge < time.Hour {
		conf += 0.02 // fixed shortly after failing, not days later
	}
	if conf > fixFactConfidenceMax {
		conf = fixFactConfidenceMax
	}
	return conf
}

// pluralY returns "y"/"ies" for 1/non-1.
func (k *Kernel) populateKnowledgeGraph(goal, output string, toolsUsed []string, agentsUsed []core.AgentRole, success bool, reflection Reflection) {
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

	// Typed fix/decision facts (local-first upgrade Phase D): promote a
	// completed task into a durable, queryable fact when the reflection
	// identified one, so future similar questions can be answered from the
	// graph (rung 2) instead of the LLM. Provenance points at the task node
	// that produced the fact; Confidence is lower than a code-index fact
	// (1.0) since this is derived from episodic outcome/keyword signal, not
	// AST ground truth, but still well above an unsourced concept edge. See
	// fixFactConfidence for how it's graded from corroborating evidence.
	switch reflection.Kind {
	case ReflectionFix:
		fixID := "fix:" + strutil.TruncateID(goal, 60)
		lessons := strings.Join(reflection.Lessons, "; ")
		_ = kg.AddNode(&core.KGNode{
			ID:    fixID,
			Label: strutil.Truncate(goal, 80),
			Type:  core.KGNodeFix,
			Properties: map[string]string{
				"resolution": strutil.Truncate(output, 240),
				"lessons":    lessons,
			},
			Provenance: taskID,
			Confidence: fixFactConfidence(reflection),
			LastSeen:   now,
		})
		_ = kg.Relate(taskID, fixID, core.KGRelProducedBy)
		// A prior failure was matched (findRecentFailure): link the problem
		// task to this fix so "how did we fix X" resolves with citations.
		if reflection.ProblemGoal != "" {
			problemID := "task:" + strutil.TruncateID(reflection.ProblemGoal, 60)
			if _, ok := kg.GetNode(problemID); ok {
				_ = kg.Relate(problemID, fixID, core.KGRelFixedBy)
			}
		}
	case ReflectionDecision:
		decisionID := "decision:" + strutil.TruncateID(goal, 60)
		lessons := strings.Join(reflection.Lessons, "; ")
		_ = kg.AddNode(&core.KGNode{
			ID:    decisionID,
			Label: strutil.Truncate(goal, 80),
			Type:  core.KGNodeDecision,
			Properties: map[string]string{
				"rationale": lessons,
			},
			Provenance: taskID,
			Confidence: decisionFactConfidence,
			LastSeen:   now,
		})
		_ = kg.Relate(decisionID, taskID, core.KGRelDecidedBecause)
	}

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
// worthPersistingSemantic decides whether a task outcome is durable knowledge
// worth writing to the semantic tier. It keeps outcomes that represent real,
// reusable work — tools were used (found info / changed files), the output
// contains code, or the reflection identified a fix or decision — and drops
// plain conversational answers (a trivia Q&A that used no tools) so the
// semantic store stays a knowledge base rather than a chat log.
func worthPersistingSemantic(output string, toolsUsed []string, reflection Reflection) bool {
	if len(toolsUsed) > 0 {
		return true
	}
	if reflection.Kind == ReflectionFix || reflection.Kind == ReflectionDecision {
		return true
	}
	return looksLikeCode(output)
}

// looksLikeCode reports whether text likely contains code worth remembering: a
// fenced block, or several code-ish signals (braces/semicolons/common
// keywords). Deliberately lenient — a false positive just keeps one extra
// entry, while the goal is only to distinguish "produced code" from "plain
// prose chat reply".
func looksLikeCode(text string) bool {
	// Strong signals: a fenced block or a declaration keyword rarely appears in
	// plain prose — any one is enough.
	for _, strong := range []string{"```", "func ", "def ", "class ", "function ", "public class", "#include", "=> {"} {
		if strings.Contains(text, strong) {
			return true
		}
	}
	// Weak signals need corroboration (two or more) to avoid flagging prose.
	weak := 0
	for _, tok := range []string{"return ", "import ", "const ", "var ", "=>", "){", "});", ";\n", "()"} {
		if strings.Contains(text, tok) {
			weak++
		}
	}
	return weak >= 2
}

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
