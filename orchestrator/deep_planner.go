package orchestrator

// deep_planner.go — the unified planning phase.
//
// This replaces BOTH prior decomposition paths (the RolePlanner sub-agent in
// dag_executor.go and, eventually, the server's generatePlanWorkflow): one
// planner, one output shape (plan.Graph), always executed by the PRIMARY
// model (router.PlannerRoute — never the local-offload intercept).
//
// Planning effort is itself cost-governed (the adaptive depth governor):
//   - light: ONE primary call → JSON graph. For moderate tasks the plan
//     isn't worth more than one call.
//   - deep:  decompose with an explicit extended-thinking scaffold, then an
//     adversarial SELF-REVIEW pass that checks the draft for missing edges,
//     false parallelism, and unverifiable acceptance criteria. Two calls,
//     sequential (free-tier RPM-safe by construction).
//
// Both depths get one automatic repair round-trip when the emitted JSON is
// unparsable or structurally invalid (cycles, unknown deps).

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/plan"
)

// PlanDepth is the planning effort level chosen by the depth governor.
type PlanDepth string

const (
	PlanDepthLight PlanDepth = "light"
	PlanDepthDeep  PlanDepth = "deep"
)

// decidePlanDepth picks the planning effort for a non-trivial goal.
// Planning is high-leverage but not free: a deep pass costs an extra primary
// call, so it is spent only where execution cost dwarfs it — complex goals,
// active project work, or an explicit override.
func decidePlanDepth(goal string, complexity int, hasProject bool, override string) PlanDepth {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "light":
		return PlanDepthLight
	case "deep":
		return PlanDepthDeep
	}
	if complexity >= 8 {
		return PlanDepthDeep
	}
	if hasProject && complexity >= 6 {
		return PlanDepthDeep
	}
	return PlanDepthLight
}

const planWireFormat = `{"summary":"<1-3 sentence approach>","tasks":[{"name":"<short-slug>","goal":"<specific, standalone-executable instruction>","agent":"worker|research|qa|critic|security|ops","deps":["<names of tasks this needs output from>"],"priority":"high|normal|low","acceptance":["<verifiable completion check>"],"artifacts":["<expected file path>"],"est_complexity":<1-10>,"parallel_safe":<true|false>}]}`

func planSystemPrompt(depth PlanDepth) string {
	var sb strings.Builder
	sb.WriteString("You are the Planning Engine — the strongest model in this system doing its highest-leverage work: decomposing a goal into an executable task graph.\n\n")
	sb.WriteString("Reason through, in order:\n")
	sb.WriteString("1. What is the real deliverable? What does verifiably \"done\" look like?\n")
	sb.WriteString("2. The MINIMAL set of tasks (usually 2-8) that produces it. Each task costs a full agent run — prefer fewer, larger tasks over many small ones.\n")
	sb.WriteString("3. True dependencies only: never serialize tasks that could run in parallel, and never parallelize tasks that write the same files (set parallel_safe=false when a task touches shared state).\n")
	sb.WriteString("4. The right agent per task: research=info gathering, worker=implementation, qa=tests, critic=review, security=risk, ops=deployment.\n")
	sb.WriteString("5. Per task: acceptance criteria a machine or reviewer could verify, and concrete artifact paths where files are produced.\n")
	sb.WriteString("6. est_complexity 1-10 per task — this drives model placement (low → cheap/local model, high → strongest model), so estimate honestly.\n\n")
	if depth == PlanDepthDeep {
		sb.WriteString("First write your full reasoning inside <analysis>...</analysis> — be exhaustive: restate the goal, enumerate alternatives, risks, file-collision constraints, and ordering constraints, then decide. After </analysis>, output ONLY a single JSON object:\n")
	} else {
		sb.WriteString("Output ONLY a single JSON object, no prose before or after:\n")
	}
	sb.WriteString(planWireFormat)
	return sb.String()
}

const planReviewPrompt = `You are adversarially reviewing your own draft plan before the user sees it. Check for:
- Missing dependency edges (a task needs another task's output but doesn't list it in deps)
- False parallelism (independent-looking tasks that write the same file or state)
- Task goals that are NOT executable standalone by an agent that sees only that goal text
- Unverifiable or missing acceptance criteria
- Over-decomposition (merge steps one agent should do together) or missing work the goal requires
- Wrong agent roles or dishonest est_complexity values
Fix every problem you find. Output ONLY the corrected FINAL plan as a single JSON object in the same format — no prose, no analysis.`

// deepPlan runs the unified planning phase and returns a validated graph.
func (k *Kernel) deepPlan(ctx context.Context, goal string, depth PlanDepth) (*plan.Graph, error) {
	client, modelName, err := k.router.PlannerRoute()
	if err != nil {
		return nil, err
	}
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("planner", "thinking",
			fmt.Sprintf("Planning (%s) on primary model %s", depth, modelName))
	}

	out, err := k.planCall(ctx, client, modelName, planSystemPrompt(depth), "Goal:\n"+goal, depth)
	if err != nil {
		return nil, err
	}
	g, err := k.parseOrRepair(ctx, client, modelName, out, goal, depth)
	if err != nil {
		return nil, err
	}

	// Deep depth: adversarial self-review of the normalized draft.
	if depth == PlanDepthDeep {
		if k.emitter != nil {
			k.emitter.EmitTaskUpdate("planner", "reviewing", "Self-reviewing draft plan")
		}
		reviewIn := "Goal:\n" + goal + "\n\nDraft plan:\n" + g.TasksJSON()
		reviewOut, rerr := k.planCall(ctx, client, modelName, planReviewPrompt, reviewIn, depth)
		if rerr == nil {
			if reviewed, perr := k.parseOrRepair(ctx, client, modelName, reviewOut, goal, depth); perr == nil {
				g = reviewed
			}
			// A failed review parse keeps the validated draft — the review is
			// an enhancement, never a point of failure.
		}
	}

	g.Depth = string(depth)
	g.CreatedBy = modelName
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("planner", "planned",
			fmt.Sprintf("Plan ready: %d task(s) in %d wave(s)", len(g.Nodes), len(g.Waves())))
	}
	return g, nil
}

// revisePlan regenerates the pending plan incorporating user feedback.
// Task IDs stay stable and completed tasks whose goals are unchanged keep
// their status/output, so an approved-and-partially-run plan can be revised
// without redoing finished work.
func (k *Kernel) revisePlan(ctx context.Context, old *plan.Graph, feedback string) (*plan.Graph, error) {
	client, modelName, err := k.router.PlannerRoute()
	if err != nil {
		return nil, err
	}
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("planner", "revising", "Revising plan from user feedback")
	}
	sys := planSystemPrompt(PlanDepthLight) +
		"\n\nYou are REVISING an existing plan based on user feedback. Keep the id of every task you don't change. Apply the feedback faithfully — add, remove, merge, or modify tasks as it requires."
	user := "Goal:\n" + old.Goal + "\n\nCurrent plan:\n" + old.TasksJSON() + "\n\nUser feedback:\n" + feedback
	out, err := k.planCall(ctx, client, modelName, sys, user, PlanDepthLight)
	if err != nil {
		return nil, err
	}
	g, err := k.parseOrRepair(ctx, client, modelName, out, old.Goal, PlanDepthLight)
	if err != nil {
		return nil, err
	}
	// Preserve finished work for unchanged tasks.
	for _, n := range g.Nodes {
		if prev := old.Node(n.ID); prev != nil && prev.Status == core.TaskCompleted && prev.Goal == n.Goal {
			n.Status = prev.Status
			n.Output = prev.Output
		}
	}
	g.Depth = old.Depth
	g.CreatedBy = modelName
	g.Revisions = old.Revisions + 1
	return g, nil
}

// planCall is one primary-model completion for the planning phase.
func (k *Kernel) planCall(ctx context.Context, client core.LLMClient, modelName, system, user string, depth PlanDepth) (string, error) {
	temp := 0.2
	maxTok := 3000
	if depth == PlanDepthDeep {
		maxTok = 6000 // room for the <analysis> scaffold + JSON
	}
	resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
		Model: modelName,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: system},
			{Role: core.RoleUser, Content: user},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("planner returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// parseOrRepair parses planner output into a validated graph, giving the
// model ONE repair round-trip when the output is unparsable or structurally
// invalid (cycle, unknown dep, empty goals).
func (k *Kernel) parseOrRepair(ctx context.Context, client core.LLMClient, modelName, out, goal string, depth PlanDepth) (*plan.Graph, error) {
	g, perr := plan.Parse(out, goal)
	if perr == nil {
		if verr := g.Validate(); verr == nil {
			return g, nil
		} else {
			perr = verr
		}
	}
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("planner", "repairing", "Plan invalid — one repair pass: "+perr.Error())
	}
	repairSys := "Your previous plan output was invalid: " + perr.Error() +
		"\nRe-emit the corrected plan as ONLY a single JSON object in this format, no prose:\n" + planWireFormat
	fixed, err := k.planCall(ctx, client, modelName, repairSys, "Goal:\n"+goal+"\n\nYour previous output:\n"+out, depth)
	if err != nil {
		return nil, fmt.Errorf("plan repair call failed after invalid plan (%v): %w", perr, err)
	}
	g, perr2 := plan.Parse(fixed, goal)
	if perr2 != nil {
		return nil, fmt.Errorf("plan unparsable after repair: %w", perr2)
	}
	if verr := g.Validate(); verr != nil {
		return nil, fmt.Errorf("plan still invalid after repair: %w", verr)
	}
	return g, nil
}
