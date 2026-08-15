package orchestrator

import (
	"context"
	"strings"

	"github.com/darkcode/adjudicate"
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

	return k.adjudicateCtx(ctx, userGoal, consensus), nil
}

// adjudicateCtx settles a consensus round through the adjudication component.
//
// The decision itself — check the claims against the graph, and only when
// checking is silent let the two most divergent answers critique each other
// once — lives in package adjudicate. The kernel supplies the pieces (model
// manager, evidence, the bus to record on, the runtime toggle) and calls it,
// rather than containing 375 lines of it.
func (k *Kernel) adjudicateCtx(ctx context.Context, goal string, consensus *core.ConsensusResult) string {
	if consensus == nil {
		return ""
	}
	adj := adjudicate.New(k.models, k.data,
		adjudicate.WithRecorder(critiqueBus{k}),
		adjudicate.WithDebate(k.debateEnabled),
		adjudicate.WithLog(func(m string) { k.log("consensus", m) }),
	)
	res := adj.Verdict(ctx, goal, consensus)

	// How the verdict was reached is emitted, not discarded.
	//
	// Everything but Answer used to be dropped here — the method, whether an
	// exchange ran, and the transcript of it. So the one feature that makes
	// multi-model worth its cost, the record of models checking each other, was
	// computed on every consensus turn and thrown away. An exchange nobody can
	// read is indistinguishable from one that never happened.
	if k.emitter != nil {
		k.emitter.EmitConsensus(map[string]interface{}{
			"method":       res.Method,
			"debated":      res.Debated,
			"transcript":   res.Transcript,
			"note":         res.Note,
			"models":       len(consensus.Contributions),
			"contributors": contributorNames(consensus),
		}, consensus.Conflict)
	}
	return res.Answer
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

// contributorNames lists which models answered, so the interface can say who
// took part rather than only how many.
func contributorNames(c *core.ConsensusResult) []string {
	out := make([]string, 0, len(c.Contributions))
	for _, x := range c.Contributions {
		if x.Error == "" && strings.TrimSpace(x.Output) != "" {
			out = append(out, x.Model)
		}
	}
	return out
}
