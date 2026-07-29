package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/dag"
	"github.com/darkcode/plan"
)

// executePlannedGraph runs the graph's DAG, merges the results, verifies,
// records memory/learning, and emits the final output — the execution half
// of what Kernel.Execute used to inline for the DAG path. Node statuses and
// outputs are synced back into the graph, and the graph is retained for the
// server to persist (ConsumeApprovedPlan).
func (k *Kernel) executePlannedGraph(ctx context.Context, g *plan.Graph, recallBlock string) (string, error) {
	goal := g.Goal
	d := g.ToDAG()
	k.log("execute", fmt.Sprintf("Executing plan graph: %d task(s) in %d wave(s)", len(g.Nodes), len(g.Waves())))
	if k.emitter != nil {
		k.emitter.EmitDAGUpdate(d.Summary())
	}

	results, err := k.executeDAG(ctx, d, goal)

	// Sync execution state back into the graph and retain it for the server
	// to persist to the active project — even on partial failure, so the
	// persisted graph shows exactly which tasks failed or were blocked.
	g.SyncFrom(d)
	k.mu.Lock()
	k.lastRunPlan = g
	k.mu.Unlock()
	if k.emitter != nil {
		k.emitter.EmitDAGUpdate(d.Summary())
	}

	if err != nil {
		if len(results) == 0 {
			return "", fmt.Errorf("DAG execution failed: %w", err)
		}
		// Best-effort recovery: cancellation aborted the DAG, but some
		// sub-agents did complete. Synthesize what we have instead of
		// discarding all completed work.
		k.log("execute", fmt.Sprintf("DAG execution failed (%v) — merging %d completed sub-task result(s)", err, len(results)))
		merged, mergeErr := k.mergeResults(ctx, results, goal)
		if mergeErr != nil {
			return "", fmt.Errorf("DAG execution failed: %w", err)
		}
		merged = fmt.Sprintf("[Partial result — DAG execution did not complete: %v]\n\n%s", err, merged)
		k.recordOutcome(goal, merged, results, false, "dag", 0, recallBlock)
		return merged, nil
	}

	// Merge results
	k.log("merge", "Merging sub-agent results")
	merged, err := k.mergeResults(ctx, results, goal)
	if err != nil {
		return "", fmt.Errorf("merge failed: %w", err)
	}

	// Surface failed/blocked tasks honestly: a run where T2 failed and T3
	// was blocked must not read as a clean success.
	if failed := d.FailedNodes(); len(failed) > 0 {
		var lines []string
		for _, n := range failed {
			lines = append(lines, fmt.Sprintf("- %s (%s): %s", n.ID, n.Name, firstErrLine(n.Error)))
		}
		for _, n := range d.AllNodes() {
			if n.Status == core.TaskCancelled {
				lines = append(lines, fmt.Sprintf("- %s (%s): blocked — %s", n.ID, n.Name, firstErrLine(n.Error)))
			}
		}
		merged += "\n\n⚠ Incomplete tasks:\n" + strings.Join(lines, "\n")
	}

	// Verify output (self-verification pipeline). A planned graph is
	// non-trivial by definition, so verification always runs here; found
	// issues are appended to the answer (and recorded below) rather than
	// silently logged.
	merged = k.verifyOutput(ctx, goal, merged, verifyComplexityMin)

	// Run each completed task's acceptance criteria, and REPAIR what fails.
	//
	// This used to run the checks and print the result, which made the DAG path
	// the mirror image of the loop path's old problem: it knew exactly what
	// "done" meant and could prove the work wasn't done, but had no way to act
	// on that — it reported a failing test suite and returned anyway. The loop
	// could iterate but had no target; the DAG had a target but could not
	// iterate. Handing a failed check to the loop closes both halves.
	merged = k.repairFailedAcceptance(ctx, g, merged)

	// Store episodic memory, record learning feedback + audit + knowledge
	// graph; skill extraction folds in via minSkillSuccess=2.
	k.log("store", "Storing episodic memory")
	k.log("improve", "Recording learning feedback and extracting reusable skills")
	k.recordOutcome(goal, merged, results, len(d.FailedNodes()) == 0, "dag", 2, recallBlock)

	if k.emitter != nil {
		k.emitter.EmitFinalOutput(merged)
	}
	k.memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: merged})

	return merged, nil
}

func firstErrLine(s string) string {
	if s == "" {
		return "no result"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ============================================================================
// DAG EXECUTION — Run tasks respecting dependencies, parallelizing where
// possible. Dependency OUTPUTS are piped into dependents (the edge carries
// data, not just ordering), failures are honest (a failed node marks its
// descendants cancelled instead of silently counting as done), and the
// ready-loop terminates cleanly either way.
// ============================================================================

// maxDepContextChars bounds how much of each dependency's output is piped
// into a dependent agent's context, so a verbose upstream agent can't blow
// the downstream prompt budget.
const maxDepContextChars = 2400

func (k *Kernel) executeDAG(ctx context.Context, d *dag.DAG, goal string) ([]*core.SubAgentResult, error) {
	var allResults []*core.SubAgentResult
	processed := make(map[string]bool)

	journal := NewExecJournal(k.runsDir, goal)
	if n := journal.Resumable(); n > 0 {
		k.log("execute", fmt.Sprintf("Resuming: %d task(s) already completed by a previous attempt", n))
	}
	journal.Append(ExecEvent{Kind: "run_started", Name: goal})

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

		// Replay anything a previous attempt already finished, so a resumed run
		// does not re-pay for completed work.
		var pending []*core.TaskNode
		for _, node := range ready {
			out, ok := journal.Completed(node.ID)
			if !ok {
				pending = append(pending, node)
				continue
			}
			processed[node.ID] = true
			d.MarkCompleted(node.ID)
			d.SetOutput(node.ID, out)
			allResults = append(allResults, &core.SubAgentResult{
				Role: node.AgentRole, Goal: node.Goal, Success: true, Output: out,
			})
			k.log("execute", fmt.Sprintf("Task %s replayed from the previous run", node.ID))
		}
		if len(pending) == 0 {
			continue // everything in this wave came from the journal
		}
		ready = pending

		// Build agent configs for ready tasks. Each dependent receives its
		// dependencies' outputs as context — without this, edges only
		// ordered execution and every agent worked blind.
		var configs []core.SubAgentConfig
		for i, node := range ready {
			configs = append(configs, core.SubAgentConfig{
				Role:      node.AgentRole,
				Goal:      node.Goal,
				ModelTier: node.ModelTier,
				MaxTurns:  k.cfg.MaxTurns,
				Context:   dependencyContext(d, node),
				// Position in this wave. The router uses it to hand
				// concurrent workers different models, so a wave of
				// independent tasks stops queueing behind one provider —
				// they ran in parallel here and were serialised at the
				// endpoint. Slot 0 routes normally, so the wave's first
				// task keeps the primary.
				WorkerSlot: i,
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

		// Record each task's real outcome. A failed task is marked failed
		// (not completed), its transitive dependents are cancelled so the
		// loop can terminate, and its result is still collected so the
		// merge can report what went wrong.
		for i, node := range ready {
			processed[node.ID] = true
			var res *core.SubAgentResult
			if i < len(results) {
				res = results[i]
			}
			if res != nil {
				allResults = append(allResults, res)
			}
			if res != nil && res.Success {
				d.MarkCompleted(node.ID)
				d.SetOutput(node.ID, res.Output)
				journal.Append(ExecEvent{Kind: "node_completed", Node: node.ID, Name: node.Name, Output: res.Output})
				continue
			}
			errMsg := "sub-agent returned no result"
			if res != nil && res.Error != "" {
				errMsg = res.Error
			}
			d.UpdateStatus(node.ID, core.TaskFailed)
			d.SetError(node.ID, errMsg)
			journal.Append(ExecEvent{Kind: "node_failed", Node: node.ID, Name: node.Name, Error: errMsg})
			for _, blocked := range d.CancelDescendants(node.ID) {
				processed[blocked] = true
				k.log("execute", fmt.Sprintf("Task %s blocked: dependency %s failed", blocked, node.ID))
			}
			if k.emitter != nil {
				k.emitter.EmitTaskUpdate(node.ID, "failed", firstErrLine(errMsg))
			}
		}
	}

	// The run reached its end, so there is nothing left to resume.
	journal.Finish()
	return allResults, nil
}

// dependencyContext assembles the outputs of a node's completed dependencies
// into the context block passed to its sub-agent.
func dependencyContext(d *dag.DAG, node *core.TaskNode) string {
	if len(node.Dependencies) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, depID := range node.Dependencies {
		dep, ok := d.GetNode(depID)
		if !ok || dep.Output == "" {
			continue
		}
		out := dep.Output
		if len(out) > maxDepContextChars {
			out = out[:maxDepContextChars] + "\n…[truncated]"
		}
		sb.WriteString(fmt.Sprintf("### Result of prerequisite task %s (%s)\n%s\n\n", dep.ID, dep.Name, out))
	}
	if sb.Len() == 0 {
		return ""
	}
	return "Results from prerequisite tasks (build on these, do not redo them):\n\n" + sb.String()
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
				Content: "You are a result synthesizer. Merge multiple sub-agent outputs into a single coherent, well-structured answer. Remove redundancy. Resolve contradictions. Present a unified response. If some agents failed, state plainly what was NOT accomplished.",
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
