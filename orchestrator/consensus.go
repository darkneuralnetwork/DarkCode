package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
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

	return k.adjudicateCtx(ctx, userGoal, consensus), nil
}

// adjudicate settles a consensus round on structural evidence rather than on
// the synthesiser's judgement.
//
// Aggregating opinions cannot detect a confidently wrong contributor. The
// graph can: each candidate's checkable claims — this symbol exists, it lives
// in that file, this package imports that one — are verified, and a candidate
// whose claims survive better than the synthesis replaces it.
//
// The synthesis keeps ties. It saw every contribution, so it is the right
// default whenever the evidence does not actually distinguish the candidates.
func (k *Kernel) adjudicate(consensus *core.ConsensusResult) string {
	return k.adjudicateCtx(context.Background(), "", consensus)
}

// adjudicateCtx is adjudicate with the context and goal needed to fall back to
// a debate round when the graph cannot decide.
func (k *Kernel) adjudicateCtx(ctx context.Context, goal string, consensus *core.ConsensusResult) string {
	kg, ok := k.memory.KG().(*memory.KnowledgeGraph)
	if !ok || kg == nil || consensus == nil {
		return consensus.Synthesized
	}

	candidates := []string{consensus.Synthesized}
	labels := []string{"synthesis"}
	for _, c := range consensus.Contributions {
		if strings.TrimSpace(c.Output) != "" {
			candidates = append(candidates, c.Output)
			labels = append(labels, c.Model)
		}
	}
	if len(candidates) < 2 {
		return consensus.Synthesized
	}

	best, supports := kg.AdjudicateCandidates(candidates)
	if best < 0 || supports[0].Checked == 0 {
		// Nothing checkable. This branch used to return the synthesis and move
		// on, which is the one case where the models disagreeing is all the
		// information there is — and where letting them answer each other is
		// worth a call. Everything else is settled by evidence, which is both
		// cheaper and better.
		if consensus.Conflict {
			if d := k.resolveByDebate(ctx, goal, consensus); d.Ran {
				k.log("consensus", "Debate settled a conflict the graph could not check")
				return d.Resolved
			}
		}
		return consensus.Synthesized
	}

	// Report what the graph refuted in the answer we are about to return,
	// so a surviving error is visible rather than silently authoritative.
	chosen := consensus.Synthesized
	if best != 0 && supports[best].Score() > supports[0].Score() {
		chosen = candidates[best]
		k.log("consensus", fmt.Sprintf(
			"Adjudicated on structure: %s (%d/%d claims verified) over the synthesis (%d/%d)",
			labels[best], supports[best].Verified, supports[best].Checked,
			supports[0].Verified, supports[0].Checked))
	}
	if wrong := supports[best].Contradicted(); len(wrong) > 0 && best == 0 {
		var lines []string
		for _, c := range wrong {
			lines = append(lines, "- "+c.Detail)
		}
		chosen += "\n\n_⚠ The code graph contradicts part of this answer:_\n" + strings.Join(lines, "\n")
	}
	return chosen
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

	return k.adjudicateCtx(ctx, userGoal, consensus), nil
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
