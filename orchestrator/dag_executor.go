package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/agents"
	"github.com/darkcode/core"
	"github.com/darkcode/dag"
)

func (k *Kernel) planAndDecompose(ctx context.Context, goal string) (*dag.DAG, error) {
	cfg := core.SubAgentConfig{
		Role:      core.RolePlanner,
		Goal:      goal, // augmentedGoal already contains conversation history and project context
		ModelTier: core.ModelTierReasoning,
		MaxTurns:  3, // planner doesn't need many turns
	}

	agent, err := k.factory.Spawn(ctx, cfg)
	if err != nil {
		return nil, err
	}

	result, err := agent.Execute(ctx)
	if err != nil {
		return nil, err
	}

	// Parse planner output into tasks
	tasks := agents.ParsePlannerOutput(result.Output)
	if len(tasks) == 0 {
		// If planner didn't produce structured tasks, try to parse the goal
		// as a single task
		return nil, fmt.Errorf("planner produced no tasks")
	}

	// Convert to DAG
	d := agents.PlannerTasksToDAG(tasks)

	// Emit the plan
	if k.emitter != nil {
		k.emitter.EmitTaskUpdate("planner", "planned",
			fmt.Sprintf("DAG with %d tasks", d.NodeCount()))
	}

	return d, nil
}

// ============================================================================
// DAG EXECUTION — Run tasks respecting dependencies, parallelizing where possible
// ============================================================================

func (k *Kernel) executeDAG(ctx context.Context, d *dag.DAG, goal string) ([]*core.SubAgentResult, error) {
	var allResults []*core.SubAgentResult
	processed := make(map[string]bool)

	for {
		// Get all tasks that are ready to run (all deps satisfied)
		ready := d.GetReadyTasks(processed)
		if len(ready) == 0 {
			// Check if everything is done
			allDone := true
			for _, node := range d.Nodes() {
				if !processed[node.ID] {
					allDone = false
					break
				}
			}
			if allDone {
				break
			}
			// Deadlock — unprocessed nodes with unsatisfied deps
			return allResults, fmt.Errorf("DAG deadlock: unresolvable dependencies")
		}

		// Build agent configs for ready tasks
		var configs []core.SubAgentConfig
		for _, node := range ready {
			configs = append(configs, core.SubAgentConfig{
				Role:      node.AgentRole,
				Goal:      node.Goal,
				ModelTier: node.ModelTier,
				MaxTurns:  k.cfg.MaxTurns,
			})
		}

		// Execute all ready tasks concurrently (or serially, in Sequential mode).
		if k.emitter != nil && len(configs) > 1 {
			mode := "parallel"
			if k.resolveSequential() {
				mode = "sequential"
			}
			k.emitter.EmitTaskUpdate("executor", "running",
				fmt.Sprintf("Running %d tasks in %s", len(configs), mode))
		}

		results := k.executor.ExecuteAll(ctx, configs)

		if ctx.Err() != nil {
			// Preserve whatever completed before cancellation so the caller
			// can attempt a best-effort merge instead of losing all work.
			return allResults, ctx.Err()
		}

		// Mark tasks as processed and collect results
		for i, node := range ready {
			processed[node.ID] = true
			d.MarkCompleted(node.ID)
			if i < len(results) && results[i] != nil {
				allResults = append(allResults, results[i])
			}
		}
	}

	return allResults, nil
}

// ============================================================================
// RESULT MERGING — Combine sub-agent outputs into a coherent answer
// ============================================================================

func (k *Kernel) mergeResults(ctx context.Context, results []*core.SubAgentResult, goal string) (string, error) {
	if len(results) == 0 {
		return "", fmt.Errorf("no results to merge")
	}

	// If only one result, return it directly
	if len(results) == 1 {
		return results[0].Output, nil
	}

	// Check if consensus mode is active
	if k.router.GetMode() == core.RouteConsensus {
		return k.mergeWithConsensus(ctx, results, goal)
	}

	// Default merge: use the reasoning model to synthesize
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Goal: %s\n\n", goal))
	sb.WriteString("Sub-agent results:\n\n")
	for i, r := range results {
		status := "success"
		if !r.Success {
			status = "failed: " + r.Error
		}
		sb.WriteString(fmt.Sprintf("--- Agent %d (%s) [%s] ---\n", i+1, r.Role, status))
		sb.WriteString(r.Output)
		sb.WriteString("\n\n")
	}

	// Use reasoning model to synthesize
	client, modelName, err := k.router.Route(core.ModelTierReasoning, 8, goal)
	if err != nil {
		// Fallback: just concatenate
		return sb.String(), nil
	}

	temp := 0.5
	maxTok := 4000
	req := &core.CompletionRequest{
		Model: modelName,
		Messages: []core.Message{
			{
				Role:    core.RoleSystem,
				Content: "You are a result synthesizer. Merge multiple sub-agent outputs into a single coherent, well-structured answer. Remove redundancy. Resolve contradictions. Present a unified response.",
			},
			{
				Role:    core.RoleUser,
				Content: sb.String(),
			},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	resp, err := client.ChatCompletion(ctx, req)
	if err != nil {
		return sb.String(), nil // fallback to raw concatenation
	}

	if len(resp.Choices) > 0 {
		return resp.Choices[0].Message.Content, nil
	}

	return sb.String(), nil
}
