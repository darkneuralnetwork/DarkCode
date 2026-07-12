package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/dag"
	"github.com/darkcode/llm"
	"github.com/darkcode/router"
	"github.com/darkcode/tools"
	"github.com/darkcode/ui"
)

// ============================================================================
// LAYER 5 — SUB-AGENT SYSTEM
// Specialized agents that the Orchestrator spawns for specific tasks.
// Each agent type has its own system prompt, model tier, and tool access.
// ============================================================================

var agentCounter int64

func nextAgentID() string {
	return fmt.Sprintf("agent_%d", atomic.AddInt64(&agentCounter, 1))
}

type ErrorHandler interface {
	Handle(err error, history []core.Message) (bool, []core.Message)
}

// SubAgent is a specialized agent that can be spawned by the orchestrator.
type SubAgent struct {
	ID        string
	Role      core.AgentRole
	Goal      string
	Config    core.SubAgentConfig
	router    core.ModelRouter
	registry  core.ToolRegistry
	emitter   *ui.EventEmitter
	errMgr    ErrorHandler
	messages  []core.Message
	startTime time.Time
}

// AgentFactory creates sub-agents with the right configuration per role.
type AgentFactory struct {
	router   core.ModelRouter
	registry core.ToolRegistry
	emitter  *ui.EventEmitter
	errMgr   ErrorHandler
}

// NewAgentFactory creates a factory for spawning sub-agents.
func NewAgentFactory(rtr core.ModelRouter, reg core.ToolRegistry, emitter *ui.EventEmitter, errMgr ErrorHandler) *AgentFactory {
	return &AgentFactory{
		router:   rtr,
		registry: reg,
		emitter:  emitter,
		errMgr:   errMgr,
	}
}

// Spawn creates a new sub-agent with the given configuration.
func (f *AgentFactory) Spawn(ctx context.Context, cfg core.SubAgentConfig) (*SubAgent, error) {
	agent := &SubAgent{
		ID:        nextAgentID(),
		Role:      cfg.Role,
		Goal:      cfg.Goal,
		Config:    cfg,
		router:    f.router,
		registry:  f.registry,
		emitter:   f.emitter,
		errMgr:    f.errMgr,
		startTime: time.Now(),
	}

	// Build role-specific system prompt
	systemPrompt := buildAgentSystemPrompt(cfg.Role, cfg.Goal, cfg.Context)
	agent.messages = []core.Message{
		{
			Role:    core.RoleSystem,
			Content: systemPrompt,
		},
		{
			Role:    core.RoleUser,
			Content: cfg.Goal,
		},
	}

	// Emit spawn event
	if f.emitter != nil {
		f.emitter.EmitAgentSpawn(cfg.Role, cfg.Goal)
	}

	return agent, nil
}

// Execute runs the agent's task to completion. It uses the model router
// to select the appropriate model, then runs a conversation loop with
// tool use until the agent produces a final answer.
func (a *SubAgent) Execute(ctx context.Context) (*core.SubAgentResult, error) {
	maxTurns := a.Config.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	complexity := router.AssessComplexity(a.Goal)
	client, modelName, err := a.router.Route(a.Config.ModelTier, complexity, a.Goal)
	if err != nil {
		return a.failResult(err), err
	}

	// Dynamically mount coding LoRA if using a local model manager for a coding task
	if lm, ok := client.(core.LoRAManager); ok {
		q := strings.ToLower(a.Goal)
		if strings.Contains(q, "code") || strings.Contains(q, "implement") || strings.Contains(q, "function") || strings.Contains(q, "script") {
			_ = lm.MountLoRA("coding", 1.0)
			defer lm.MountLoRA("coding", 0.0)
		}
	}

	var allToolCalls []core.ToolCall
	var lastCallSig string
	var repeatCount int

	for turn := 0; turn < maxTurns; turn++ {
		if ctx.Err() != nil {
			err := ctx.Err()
			return a.failResult(err), err
		}

		// Emit thinking status
		if a.emitter != nil {
			a.emitter.EmitTaskUpdate(a.ID, "running",
				fmt.Sprintf("agent working (step %d)", turn+1))
		}

		// Call LLM with streaming and ErrorManager retry loop
		var resp *core.CompletionResponse
		var llmErr error

		for attempt := 0; attempt < 2; attempt++ {
			temp := 0.7
			req := &llm.CompletionRequest{
				Model:       modelName,
				Messages:    a.messages,
				Temperature: &temp,
				Tools:       a.registry.LLMSchemas().([]llm.ToolSchema),
			}

			resp, llmErr = client.ChatCompletionStream(ctx, req, &llm.StreamCallbacks{
				OnContent: func(chunk string) {
					if a.emitter != nil {
						a.emitter.Emit(core.EventTaskUpdate, chunk,
							ui.WithTaskID(a.ID), ui.WithStatus("streaming"),
							ui.WithAgent(string(a.Role)))
					}
				},
				OnToolCall: func(tc core.ToolCall) {
					if a.emitter != nil {
						a.emitter.EmitToolExecution(tc.Function.Name, "requested", tc.Function.Arguments)
					}
				},
			})

			if llmErr != nil && a.errMgr != nil {
				modified, newHist := a.errMgr.Handle(llmErr, a.messages)
				if modified {
					a.messages = newHist
					continue // Retry with modified history
				}
			}
			break
		}

		if llmErr != nil {
			return a.failResult(llmErr), llmErr
		}
		if len(resp.Choices) == 0 {
			return a.failResult(fmt.Errorf("empty response")), fmt.Errorf("empty response")
		}

		msg := resp.Choices[0].Message

		// Add assistant message to history
		a.messages = append(a.messages, core.Message{
			Role:      core.RoleAssistant,
			Content:   msg.Content,
			ToolCalls: msg.ToolCalls,
		})

		// If no tool calls, we're done
		if len(msg.ToolCalls) == 0 {
			duration := time.Since(a.startTime)
			result := &core.SubAgentResult{
				AgentID:   a.ID,
				Role:      a.Role,
				Goal:      a.Goal,
				Output:    msg.Content,
				Success:   true,
				ToolCalls: allToolCalls,
				Duration:  duration.String(),
			}
			if a.emitter != nil {
				a.emitter.EmitAgentComplete(a.Role, a.Goal, msg.Content, true)
			}
			return result, nil
		}

		// Loop protection: if the agent makes the exact same tool call sequence 3 times in a row, break out
		if len(msg.ToolCalls) > 0 {
			callSig := ""
			for _, tc := range msg.ToolCalls {
				callSig += tc.Function.Name + ":" + tc.Function.Arguments + "|"
			}
			if callSig == lastCallSig {
				repeatCount++
				if repeatCount >= 3 {
					err := fmt.Errorf("agent got stuck in a loop calling: %s", msg.ToolCalls[0].Function.Name)
					return a.failResult(err), err
				}
			} else {
				lastCallSig = callSig
				repeatCount = 0
			}
		}

		// Execute tools concurrently
		allToolCalls = append(allToolCalls, msg.ToolCalls...)
		toolResultsi := a.registry.DispatchAll(ctx, msg.ToolCalls)
		toolResults, ok := toolResultsi.([]tools.DispatchResult)
		if !ok {
			return a.failResult(fmt.Errorf("agent %s: unexpected tool result type", a.ID)), fmt.Errorf("agent %s: unexpected tool result type", a.ID)
		}

		for _, result := range toolResults {
			if a.emitter != nil {
				a.emitter.EmitToolExecution(result.Name, "completed", result.Result)
			}

			// Format tool result
			var content string
			if result.Result != nil {
				if !result.Result.Success && result.Result.Error != "" {
					content = "Error: " + result.Result.Error
				} else if result.Result.Output != "" {
					content = result.Result.Output
				} else {
					content = "Command executed successfully with no output."
				}
			} else {
				content = "(tool returned nil result)"
			}

			a.messages = append(a.messages, core.Message{
				Role:       core.RoleTool,
				Content:    content,
				ToolCallID: result.CallID,
				Name:       result.Name,
			})
		}
	}

	// Max turns exceeded
	err = fmt.Errorf("agent exceeded max turns (%d)", maxTurns)
	return a.failResult(err), err
}

func (a *SubAgent) failResult(err error) *core.SubAgentResult {
	duration := time.Since(a.startTime)
	result := &core.SubAgentResult{
		AgentID:  a.ID,
		Role:     a.Role,
		Goal:     a.Goal,
		Success:  false,
		Error:    err.Error(),
		Duration: duration.String(),
	}
	if a.emitter != nil {
		a.emitter.EmitAgentComplete(a.Role, a.Goal, err.Error(), false)
	}
	return result
}

// buildAgentSystemPrompt creates a role-specific system prompt.
func buildAgentSystemPrompt(role core.AgentRole, goal, context string) string {
	var sb strings.Builder

	switch role {
	case core.RoleExecutive:
		sb.WriteString("You are an Executive Agent. You provide high-level control and goal tracking.\n")
		sb.WriteString("Your job is to break down complex goals into actionable sub-tasks and coordinate execution.\n")
		sb.WriteString("Focus on the big picture. Delegate details to worker agents.\n")

	case core.RolePlanner:
		sb.WriteString("You are a Planner Agent. Your job is task decomposition.\n")
		sb.WriteString("Given a goal, create a DAG of tasks with dependencies.\n")
		sb.WriteString("Output each task as:\n")
		sb.WriteString("TASK: <name> | GOAL: <description> | DEPS: <comma-separated task names> | AGENT: <worker|critic|research|qa|security|ops> | PRIORITY: <high|normal|low>\n")
		sb.WriteString("End with: PLAN_END\n")
		sb.WriteString("Consider which tasks can run in parallel (no dependencies).\n")
		sb.WriteString("Assign the right agent type: research for info gathering, qa for testing, security for risk, ops for deployment.\n")

	case core.RoleWorker:
		sb.WriteString("You are a Coding Agent. You execute implementation tasks using available tools.\n")
		sb.WriteString("You can write code, run commands, read/write files, and call APIs.\n")
		sb.WriteString("CRITICAL: Be extremely efficient. Avoid exploring the file system unnecessarily if you can create files directly. Accomplish your goal in as few tool calls as possible and DO NOT loop or perform redundant checks. Once finished, stop calling tools and provide a clear summary of what you accomplished.\n")

	case core.RoleCritic:
		sb.WriteString("You are a Critic Agent. Your job is validation and quality assurance.\n")
		sb.WriteString("Check for:\n")
		sb.WriteString("- Correctness of the solution\n")
		sb.WriteString("- Bugs and edge cases\n")
		sb.WriteString("- Hallucinations or fabricated information\n")
		sb.WriteString("- Missing requirements\n")
		sb.WriteString("Provide specific, actionable feedback. If the work is correct, say so explicitly.\n")

	case core.RoleResearch:
		sb.WriteString("You are a Research Agent. Your job is information gathering and analysis.\n")
		sb.WriteString("You specialize in:\n")
		sb.WriteString("- Searching the web and documentation for relevant information\n")
		sb.WriteString("- Analyzing codebases and identifying patterns\n")
		sb.WriteString("- Summarizing technical papers and documentation\n")
		sb.WriteString("- Gathering requirements and constraints\n")
		sb.WriteString("Cite sources when possible. Distinguish facts from interpretations.\n")
		sb.WriteString("Report confidence levels for uncertain findings.\n")

	case core.RoleQA:
		sb.WriteString("You are a QA Agent. Your job is testing and quality assurance.\n")
		sb.WriteString("You specialize in:\n")
		sb.WriteString("- Writing and running test cases\n")
		sb.WriteString("- Edge case analysis and boundary testing\n")
		sb.WriteString("- Regression testing and integration testing\n")
		sb.WriteString("- Performance benchmarking\n")
		sb.WriteString("- Code review for correctness and maintainability\n")
		sb.WriteString("Report all findings with severity levels: critical, major, minor, info.\n")

	case core.RoleSecurity:
		sb.WriteString("You are a Security Agent. Your job is risk analysis and vulnerability detection.\n")
		sb.WriteString("You specialize in:\n")
		sb.WriteString("- Identifying security vulnerabilities in code and configurations\n")
		sb.WriteString("- Risk scoring for proposed actions and changes\n")
		sb.WriteString("- Checking for injection attacks, data leaks, and privilege escalation\n")
		sb.WriteString("- Reviewing dependencies for known vulnerabilities\n")
		sb.WriteString("- Ensuring compliance with security best practices\n")
		sb.WriteString("Classify each finding by risk level: low, medium, high, critical.\n")

	case core.RoleOps:
		sb.WriteString("You are an Ops Agent. Your job is deployment and operational tasks.\n")
		sb.WriteString("You specialize in:\n")
		sb.WriteString("- Deployment planning and execution\n")
		sb.WriteString("- Health checks and system monitoring\n")
		sb.WriteString("- Infrastructure configuration and management\n")
		sb.WriteString("- CI/CD pipeline setup and maintenance\n")
		sb.WriteString("- Log analysis and incident response\n")
		sb.WriteString("Always verify system state before and after changes.\n")

	case core.RoleCompression:
		sb.WriteString("You are a Compression Agent. Compress context to essential signals only.\n")
		sb.WriteString("Remove redundancy. Preserve file paths, errors, and key decisions.\n")

	case core.RoleUI:
		sb.WriteString("You are a UI Agent. Render execution state as structured UI events.\n")
		sb.WriteString("Make all reasoning observable and transparent.\n")

	default:
		sb.WriteString("You are a sub-agent. Complete your assigned task.\n")
	}

	sb.WriteString(fmt.Sprintf("\nYour assigned goal: %s\n", goal))

	if context != "" {
		sb.WriteString(fmt.Sprintf("\nContext:\n%s\n", context))
	}

	return sb.String()
}

// ============================================================================
// PLANNER AGENT — Specialized for DAG creation
// ============================================================================

// PlannerResult is the structured output of a planner agent.
type PlannerResult struct {
	Tasks []PlannerTask `json:"tasks"`
}

// PlannerTask is a single task parsed from the planner's output.
type PlannerTask struct {
	Name     string            `json:"name"`
	Goal     string            `json:"goal"`
	Deps     []string          `json:"dependencies"`
	Agent    core.AgentRole    `json:"agent"`
	Priority core.TaskPriority `json:"priority"`
}

// ParsePlannerOutput extracts structured tasks from the planner agent's text output.
func ParsePlannerOutput(text string) []PlannerTask {
	var tasks []PlannerTask
	lines := strings.Split(text, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "TASK:") {
			continue
		}
		if strings.Contains(line, "PLAN_END") {
			break
		}

		task := parseTaskLine(line)
		if task.Name != "" {
			tasks = append(tasks, task)
		}
	}

	return tasks
}

func parseTaskLine(line string) PlannerTask {
	task := PlannerTask{
		Agent:    core.RoleWorker,
		Priority: core.PriorityNormal,
	}

	// Parse: TASK: <name> | GOAL: <desc> | DEPS: <deps> | AGENT: <type> | PRIORITY: <level>
	parts := strings.Split(line, "|")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "TASK:") {
			task.Name = strings.TrimSpace(strings.TrimPrefix(part, "TASK:"))
		} else if strings.HasPrefix(part, "GOAL:") {
			task.Goal = strings.TrimSpace(strings.TrimPrefix(part, "GOAL:"))
		} else if strings.HasPrefix(part, "DEPS:") {
			depsStr := strings.TrimSpace(strings.TrimPrefix(part, "DEPS:"))
			if depsStr != "" && depsStr != "none" {
				for _, d := range strings.Split(depsStr, ",") {
					d = strings.TrimSpace(d)
					if d != "" {
						task.Deps = append(task.Deps, d)
					}
				}
			}
		} else if strings.HasPrefix(part, "AGENT:") {
			agentStr := strings.TrimSpace(strings.TrimPrefix(part, "AGENT:"))
			switch strings.ToLower(agentStr) {
			case "critic":
				task.Agent = core.RoleCritic
			case "planner":
				task.Agent = core.RolePlanner
			case "executive":
				task.Agent = core.RoleExecutive
			case "research":
				task.Agent = core.RoleResearch
			case "qa":
				task.Agent = core.RoleQA
			case "security":
				task.Agent = core.RoleSecurity
			case "ops":
				task.Agent = core.RoleOps
			default:
				task.Agent = core.RoleWorker
			}
		} else if strings.HasPrefix(part, "PRIORITY:") {
			priStr := strings.TrimSpace(strings.TrimPrefix(part, "PRIORITY:"))
			switch strings.ToLower(priStr) {
			case "critical":
				task.Priority = core.PriorityCritical
			case "high":
				task.Priority = core.PriorityHigh
			case "low":
				task.Priority = core.PriorityLow
			default:
				task.Priority = core.PriorityNormal
			}
		}
	}

	return task
}

// PlannerTasksToDAG converts planner output into a DAG.
func PlannerTasksToDAG(tasks []PlannerTask) *dag.DAG {
	d := dag.NewDAG()

	// First pass: create all nodes (so dependencies can be found)
	nameToID := make(map[string]string)
	for i, task := range tasks {
		id := fmt.Sprintf("task_%d", i+1)
		nameToID[task.Name] = id
	}

	// Second pass: add nodes with resolved dependencies
	for i, task := range tasks {
		id := fmt.Sprintf("task_%d", i+1)

		var deps []string
		for _, depName := range task.Deps {
			if depID, ok := nameToID[depName]; ok {
				deps = append(deps, depID)
			}
		}

		node := &core.TaskNode{
			ID:           id,
			Name:         task.Name,
			Goal:         task.Goal,
			Status:       core.TaskPending,
			Priority:     task.Priority,
			Dependencies: deps,
			AgentRole:    task.Agent,
			ModelTier:    tierForAgent(task.Agent),
		}

		_ = d.AddNode(node) // error only on duplicate/missing dep, which we handle
	}

	return d
}

// tierForAgent selects the appropriate model tier for an agent role.
func tierForAgent(role core.AgentRole) core.ModelTier {
	switch role {
	case core.RoleExecutive, core.RolePlanner:
		return core.ModelTierReasoning
	case core.RoleCritic, core.RoleQA:
		return core.ModelTierCritic
	case core.RoleSecurity:
		return core.ModelTierReasoning
	case core.RoleResearch:
		return core.ModelTierCoding
	case core.RoleOps:
		return core.ModelTierCoding
	case core.RoleCompression:
		return core.ModelTierFast
	case core.RoleUI:
		return core.ModelTierFast
	default:
		return core.ModelTierCoding
	}
}

// ============================================================================
// CONCURRENT AGENT EXECUTION
// ============================================================================

// ConcurrentExecutor runs multiple sub-agents in parallel using goroutines.
type ConcurrentExecutor struct {
	factory       *AgentFactory
	maxConcurrent int
	emitter       *ui.EventEmitter
	mu            sync.Mutex // guards maxConcurrent (hot-toggled by the kernel)
}

// NewConcurrentExecutor creates a parallel agent executor.
func NewConcurrentExecutor(factory *AgentFactory, maxConcurrent int, emitter *ui.EventEmitter) *ConcurrentExecutor {
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}
	return &ConcurrentExecutor{
		factory:       factory,
		maxConcurrent: maxConcurrent,
		emitter:       emitter,
	}
}

// SetMaxConcurrent hot-toggles the concurrency limit at runtime. The kernel
// calls this from the resolved execution profile: 1 for Sequential (serial
// sub-agent execution — free-tier-safe), or cfg.MaxConcurrent for Parallel.
// Takes effect on the next ExecuteAll call (the semaphore is sized per call).
func (e *ConcurrentExecutor) SetMaxConcurrent(n int) {
	if n < 1 {
		n = 1
	}
	e.mu.Lock()
	e.maxConcurrent = n
	e.mu.Unlock()
}

// MaxConcurrent returns the current concurrency limit (for telemetry/status).
func (e *ConcurrentExecutor) MaxConcurrent() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxConcurrent
}

// ExecuteAll runs multiple agent configurations concurrently and returns all results.
// Respects maxConcurrent limit using a semaphore.
func (e *ConcurrentExecutor) ExecuteAll(ctx context.Context, configs []core.SubAgentConfig) []*core.SubAgentResult {
	var mu sync.Mutex
	results := make([]*core.SubAgentResult, len(configs))
	e.mu.Lock()
	conc := e.maxConcurrent
	e.mu.Unlock()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, cfg := range configs {
		wg.Add(1)
		go func(idx int, config core.SubAgentConfig) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			agent, err := e.factory.Spawn(ctx, config)
			if err != nil {
				results[idx] = &core.SubAgentResult{
					AgentID: "failed",
					Role:    config.Role,
					Goal:    config.Goal,
					Success: false,
					Error:   err.Error(),
				}
				return
			}

			result, err := agent.Execute(ctx)
			if err != nil && result == nil {
				result = &core.SubAgentResult{
					AgentID: agent.ID,
					Role:    config.Role,
					Goal:    config.Goal,
					Success: false,
					Error:   err.Error(),
				}
			}
			mu.Lock()
			results[idx] = result
			mu.Unlock()
		}(i, cfg)
	}

	wg.Wait()
	return results
}

// (buildToolSchemas was removed: the tools.Registry now owns the
// ToolDef → llm.ToolSchema mapping via Registry.LLMSchemas(), which this
// package and agent/ and loop/ all call instead of maintaining identical
// private copies.)
