package orchestrator

import (
	"context"
	"strings"

	"github.com/darkcode/core"
)

func (k *Kernel) runConsensus(ctx context.Context, userGoal string, preamble string) (string, error) {
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("consensus", "starting", "Multi-model consensus round")
	}

	// Build messages from STM history + the new goal. An optional preamble
	// (e.g. the General-mode "no tools" constraint) is prepended as a system
	// message so every contributing model and the synthesizer share the same
	// operating context and cannot hallucinate capabilities they lack.
	messages := k.memory.STMGet()
	if preamble != "" {
		messages = append([]core.Message{{Role: core.RoleSystem, Content: preamble}}, messages...)
	}
	messages = append(messages, core.Message{Role: core.RoleUser, Content: userGoal})

	consensus, err := k.router.Consensus(ctx, messages, userGoal)
	if err != nil {
		return "", err
	}

	return consensus.Synthesized, nil
}

// runConsensusOnOutput runs a consensus synthesis round on an already-produced
// answer (typically from the agentic loop). The non-primary models review the
// answer from their role perspectives (critic, skeptic, knowledge_booster, …),
// and the primary model synthesizes a refined final answer. This lets consensus
// mode enhance tool-using agentic output WITHOUT bypassing tool execution —
// the tools already ran; this just refines the final answer with multi-model
// perspectives.
//
// toolTrace is the agentic loop's log of executed tools + their real results.
// It is injected into the review prompt so the refiners are grounded in what
// actually happened and cannot hallucinate that the agent cannot take action
// (the prior bug: a "skeptic" model overrode a successful write_file with
// "I cannot create files", and the synthesizer adopted that hallucination).
func (k *Kernel) runConsensusOnOutput(ctx context.Context, userGoal, output, toolTrace string) (string, error) {
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("consensus", "synthesis", "Multi-model consensus synthesis on agentic output")
	}

	// Build messages: prior conversation history + the original question +
	// the agentic loop's answer + a review request. Each non-primary model
	// sees these and responds from its role persona; the primary then
	// synthesizes all reviews into the final. STM is prepended (matching
	// runConsensus's pattern above) so a persona model reviewing a later
	// turn — e.g. a short follow-up referencing something from earlier in
	// the conversation — is grounded in the actual history instead of only
	// this one isolated exchange, which previously produced confused
	// "I don't understand" reviews on legitimate context-dependent follow-ups.
	reviewReq := "Review the above answer from your assigned role's perspective. Provide your assessment, corrections, or enhancements."
	if strings.TrimSpace(toolTrace) != "" {
		reviewReq = "The agent has ALREADY executed these tools with REAL results during this task:\n" +
			strings.TrimSpace(toolTrace) +
			"\n\nThe answer above is grounded in those real actions. Do NOT claim the agent cannot perform actions or lacks tool/filesystem access. Review only for accuracy, completeness, and clarity, and refine the wording."
	}

	messages := k.memory.STMGet()
	messages = append(messages,
		core.Message{Role: core.RoleUser, Content: userGoal},
		core.Message{Role: core.RoleAssistant, Content: output},
		core.Message{Role: core.RoleUser, Content: reviewReq},
	)

	consensus, err := k.router.Consensus(ctx, messages, userGoal)
	if err != nil {
		return "", err
	}

	return consensus.Synthesized, nil
}

func (k *Kernel) mergeWithConsensus(ctx context.Context, results []*core.SubAgentResult, goal string) (string, error) {
	// Build messages from results
	var content strings.Builder
	for _, r := range results {
		content.WriteString(r.Output)
		content.WriteString("\n\n")
	}

	// Prepend STM (matching runConsensus/runConsensusOnOutput) so the
	// synthesis persona models see the actual conversation, not just this
	// DAG's isolated sub-agent outputs — the same context-poor path that
	// could confuse a persona model on a follow-up referencing earlier
	// turns.
	messages := k.memory.STMGet()
	messages = append(messages, core.Message{
		Role:    core.RoleUser,
		Content: "Synthesize these sub-agent results into a final answer:\n\n" + content.String(),
	})

	consensus, err := k.router.Consensus(ctx, messages, goal)
	if err != nil {
		// Fallback to simple merge
		return content.String(), nil
	}

	return consensus.Synthesized, nil
}

// ============================================================================
// EPISODIC MEMORY STORAGE
// ============================================================================
