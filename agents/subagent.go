package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/compression"
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
	"github.com/darkcode/modelport"
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
// workerRouter is the distribution-aware subset of the router. SubAgent holds
// core.ModelRouter (an interface the orchestrator owns), and RouteWorker is a
// concrete-router capability, so it is reached by assertion rather than by
// widening that interface for every implementation — including the test fakes.
type workerRouter interface {
	RouteWorker(tier core.ModelTier, complexity int, taskDesc string, slot int) (core.LLMClient, string, error)
}

// routeForSlot picks this agent's model, spreading concurrent workers across
// registered models when the router supports it.
func (a *SubAgent) routeForSlot(complexity int) (core.LLMClient, string, error) {
	if a.Config.WorkerSlot > 0 {
		if wr, ok := a.router.(workerRouter); ok {
			return wr.RouteWorker(a.Config.ModelTier, complexity, a.Goal, a.Config.WorkerSlot)
		}
	}
	return a.router.Route(a.Config.ModelTier, complexity, a.Goal)
}

func (a *SubAgent) Execute(ctx context.Context) (*core.SubAgentResult, error) {
	maxTurns := a.Config.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	complexity := router.AssessComplexity(a.Goal)
	// Workers in the same concurrent wave carry different slots so they land
	// on different models where more than one is registered — the executor ran
	// them concurrently but they all queued behind one provider. Slot 0 routes
	// exactly as before, so a single-task wave is unchanged.
	client, modelName, err := a.routeForSlot(complexity)
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
	// Per-tool call budget: the exact-repeat guard below only catches
	// byte-identical calls, so an agent that re-searches with slightly
	// reworded queries (e.g. 12 web_search calls for one factual question)
	// slips past it and burns turns + LLM calls. Once a single tool has been
	// used perToolSoftCap times we nudge the model to conclude; at
	// perToolHardCap we STOP offering tools for the rest of the run, forcing a
	// final text answer. This makes a runaway tool loop converge cheaply.
	toolNameCounts := map[string]int{}
	nudged := false
	forcedFinal := false
	const perToolSoftCap = 3
	const perToolHardCap = 5

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

		// Tool budget: decide whether to keep offering tools this turn. Once
		// any tool hits the hard cap we force a final text answer by not
		// offering tools; a one-time soft nudge earlier asks the model to
		// wrap up on its own.
		maxUses := 0
		for _, n := range toolNameCounts {
			if n > maxUses {
				maxUses = n
			}
		}
		offerTools := maxUses < perToolHardCap
		if maxUses >= perToolSoftCap && !nudged && offerTools {
			// A user turn, not a system one: see loop.nudgeRole — a system
			// message after a tool response makes Gemini reject the next call.
			a.messages = append(a.messages, core.Message{
				Role:    core.RoleUser,
				Content: "You have gathered enough information. Stop calling tools and answer the goal now, concisely, using what you already have.",
			})
			nudged = true
		}
		if !offerTools && !forcedFinal {
			a.messages = append(a.messages, core.Message{
				Role:    core.RoleUser,
				Content: "Tool budget reached. Provide your FINAL answer now as plain text using the information already gathered. Do not request any more tools.",
			})
			forcedFinal = true
		}

		// Call LLM with streaming and ErrorManager retry loop
		var resp *core.CompletionResponse
		var llmErr error

		for attempt := 0; attempt < 2; attempt++ {
			temp := 0.7
			// Bound the reply. This ran on every worker turn with no ceiling,
			// so the limit was whatever the provider defaults to — usually the
			// rest of the context window. The number comes from the one policy
			// table rather than being invented here again.
			_, maxTok, _ := modelport.PolicyFor(modelport.PurposeExecute)
			var schemas []llm.ToolSchema
			if offerTools {
				// Scoped by role: a research or critic agent is never even
				// shown a tool that changes anything. See toolscope.go.
				schemas = schemasFor(a.registry, a.Config)
			}
			// Hard context-fit guarantee before dispatch (Part 3 contract):
			// worker history grows with each tool turn, so fit to the
			// receiving client's effective window to prevent a local-model
			// "context window exceeded" fatal mid-task.
			a.messages = compression.FitClient(a.messages, client, 0, len(schemas))
			req := &llm.CompletionRequest{
				Model:       modelName,
				Messages:    a.messages,
				Temperature: &temp,
				MaxTokens:   &maxTok,
				Tools:       schemas, // nil once the tool budget is spent → forces a final answer
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

			// Recovery ladder for a context overflow that slipped past the
			// FitClient estimate (tokenizer drift): shrink hard to 75% of the
			// client's window and retry once, rather than failing the task.
			if llmErr != nil && errors.Is(llmErr, core.ErrContextTooLong) && attempt == 0 {
				window := 0
				if client != nil {
					window = client.ModelInfo().Context
				}
				if window <= 0 {
					window = compression.DefaultContextWindow
				}
				a.messages = compression.FitToWindow(a.messages, window*3/4, 0)
				continue
			}
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

		// Count per-tool usage for the budget guard above.
		for _, tc := range msg.ToolCalls {
			toolNameCounts[tc.Function.Name]++
		}

		// Enforce the per-tool cap rather than merely stopping to offer the
		// tool. Withholding a schema is a hint the model is free to ignore —
		// and models do, either by echoing a tool name from earlier in the
		// conversation or by emitting a call when none were offered at all.
		// Left un-enforced the cap did nothing in exactly the case it exists
		// for: an agent that keeps searching with slightly reworded arguments,
		// which the exact-repeat guard above cannot see.
		var spent []core.ToolCall
		var allowed []core.ToolCall
		for _, tc := range msg.ToolCalls {
			if toolNameCounts[tc.Function.Name] > perToolHardCap {
				spent = append(spent, tc)
				continue
			}
			allowed = append(allowed, tc)
		}
		for _, tc := range spent {
			a.messages = append(a.messages, core.Message{
				Role:       core.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content: "Error: the budget for " + tc.Function.Name + " is spent (" +
					fmt.Sprint(perToolHardCap) + " calls). Answer from what you already have.",
			})
		}
		if len(allowed) == 0 {
			continue // everything this turn was over budget; let the model reply
		}
		msg.ToolCalls = allowed

		// Execute tools concurrently
		allToolCalls = append(allToolCalls, msg.ToolCalls...)
		// Enforce the role's scope at dispatch, not just at offer time. Not
		// showing a tool is ergonomics; refusing to run it is the boundary. A
		// model that invents `terminal` out of nothing — or is talked into it
		// by a page it was asked to summarise — is turned away here by the
		// registry's existing read-only check.
		dispatchCtx := ctx
		if ScopeFor(a.Role) == ScopeReadOnly {
			dispatchCtx = context.WithValue(ctx, core.ReadOnlyToolsKey, true)
			dispatchCtx = context.WithValue(dispatchCtx, core.ReadOnlyReasonKey,
				"the "+string(a.Role)+" role observes and reports; it has no write authority. "+
					"Complete the task with read-only tools, or report what a writing agent would need to do.")
		}
		toolResultsi := a.registry.DispatchAll(dispatchCtx, msg.ToolCalls)
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

			toolName := result.Name
			if toolName == "" {
				toolName = "unknown_tool"
			}
			a.messages = append(a.messages, core.Message{
				Role: core.RoleTool,
				// Same rendering as the ReAct loop — a sub-agent used to see
				// the untrimmed result while the loop saw 4 KB of it.
				Content:    a.registry.ObserveResult(toolName, content),
				ToolCallID: result.CallID,
				Name:       toolName,
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
		sb.WriteString("Output ONLY a JSON array of task objects, nothing else (no prose, no markdown fences). Each object has:\n")
		sb.WriteString(`  {"name": "<short unique id>", "goal": "<what to do>", "dependencies": ["<other task names>"], "agent": "worker|critic|research|qa|security|ops", "priority": "high|normal|low"}` + "\n")
		sb.WriteString("Use an empty dependencies array for tasks that can run in parallel.\n")
		sb.WriteString("Assign the right agent type: research for info gathering, qa for testing, security for risk, ops for deployment; worker for implementation.\n")
		sb.WriteString(`Example: [{"name":"impl","goal":"write the parser","dependencies":[],"agent":"worker","priority":"high"},{"name":"test","goal":"add tests","dependencies":["impl"],"agent":"qa","priority":"normal"}]` + "\n")

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
