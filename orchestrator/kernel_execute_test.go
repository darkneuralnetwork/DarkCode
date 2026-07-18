package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/metrics"
)

// promptRecorder is a fakeLLMClient respFunc that records every system-prompt
// it sees (thread-safe, since the DAG/consensus paths call concurrently) and
// returns a planner-format response when handed the planner prompt so the DAG
// path actually builds a DAG instead of falling back to direct execution.
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (p *promptRecorder) respFunc(idx int, req *core.CompletionRequest) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	isPlanner := false
	for _, m := range req.Messages {
		if m.Role == core.RoleSystem {
			c := m.ContentString()
			p.prompts = append(p.prompts, c)
			if strings.Contains(c, "Planner Agent") {
				isPlanner = true
			}
		}
	}
	if isPlanner {
		return plannerResponse(plannerTask{name: "t1", goal: "do the thing"})
	}
	return "task output"
}

func (p *promptRecorder) sawPrompt(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.prompts {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestExecuteDispatchGeneralMode(t *testing.T) {
	rec := &promptRecorder{}
	client := &fakeLLMClient{name: "fake", respFunc: rec.respFunc}
	deps := newTestKernel(t, client)

	// chat_mode="general" disables tools for the request.
	restore := deps.Kernel.ApplyRequestOverrides("", "", "", "off", "")
	defer restore()

	// Concrete indicator ("implement") keeps it out of the clarification gate;
	// general mode then takes the no-tools path before any trivial/DAG branch.
	out, err := deps.Kernel.Execute(context.Background(), "explain how to implement HTTP caching")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty answer")
	}
	if !rec.sawPrompt("General (conversational) mode") {
		t.Error("expected the general-mode (no-tools) path to be taken")
	}
	if rec.sawPrompt("Planner Agent") || rec.sawPrompt("Agentic Loop (ReAct)") {
		t.Error("general mode must not enter the DAG or loop paths")
	}
}

func TestExecuteDispatchLoopMode(t *testing.T) {
	rec := &promptRecorder{}
	client := &fakeLLMClient{name: "fake", respFunc: rec.respFunc}
	deps := newTestKernel(t, client)

	restore := deps.Kernel.ApplyRequestOverrides("", "", "on", "on", "")
	defer restore()

	out, err := deps.Kernel.Execute(context.Background(), "add a retry helper to the http client")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty answer")
	}
	if !rec.sawPrompt("Agentic Loop (ReAct)") {
		t.Error("expected the ReAct loop path to be taken when loop mode is on")
	}
}

func TestExecuteDispatchTrivialDirect(t *testing.T) {
	rec := &promptRecorder{}
	client := &fakeLLMClient{name: "fake", respFunc: rec.respFunc}
	deps := newTestKernel(t, client)

	// Short, concrete, low-complexity, no multi-step indicators → trivial →
	// executeDirect (single worker), not the DAG planner.
	out, err := deps.Kernel.Execute(context.Background(), "read the config file")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty answer")
	}
	if !rec.sawPrompt("Coding Agent") {
		t.Error("expected the trivial-direct path to spawn a single Coding Agent worker")
	}
	if rec.sawPrompt("Planner Agent") {
		t.Error("a trivial task must not be decomposed via the DAG planner")
	}
}

func TestExecuteDispatchDAGDecomposition(t *testing.T) {
	rec := &promptRecorder{}
	client := &fakeLLMClient{name: "fake", respFunc: rec.respFunc}
	deps := newTestKernel(t, client)

	// Multi-step indicators ("and then", "step by step") force non-trivial →
	// the DAG planner path.
	out, err := deps.Kernel.Execute(context.Background(),
		"implement the auth module and then add tests for it step by step")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty answer")
	}
	if !rec.sawPrompt("Planner Agent") {
		t.Error("expected a non-trivial multi-step task to go through the DAG planner")
	}
}

func TestExecuteDispatchClarificationGate(t *testing.T) {
	client := &fakeLLMClient{name: "fake"}
	deps := newTestKernel(t, client)

	out, err := deps.Kernel.Execute(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "doesn't name anything to act on") {
		t.Errorf("out = %q, want the clarification message for a vague request", out)
	}
	if client.callCount() != 0 {
		t.Errorf("clarification path must not call the LLM, got %d calls", client.callCount())
	}
}

// TestExecuteConfidentRecallShortCircuit verifies the Phase-4 graph-first
// LLM-skip: a repeated no-tool question is answered from cache with zero LLM
// calls on the second Execute.
func TestExecuteConfidentRecallShortCircuit(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"HTTP caching stores responses to avoid refetching."}}
	deps := newTestKernel(t, client)

	restore := deps.Kernel.ApplyRequestOverrides("", "", "", "off", "") // general mode (no tools → cacheable)
	defer restore()

	q := "explain how to implement HTTP caching in detail please"
	first, err := deps.Kernel.Execute(context.Background(), q)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	callsAfterFirst := client.callCount()
	if callsAfterFirst == 0 {
		t.Fatal("first Execute should have called the LLM")
	}

	second, err := deps.Kernel.Execute(context.Background(), q)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if client.callCount() != callsAfterFirst {
		t.Errorf("second identical Execute made %d additional LLM call(s), want 0 (ConfidentRecall cache hit)",
			client.callCount()-callsAfterFirst)
	}
	if second != first {
		t.Errorf("cached answer %q != original %q", second, first)
	}
}

// TestExecuteCostGovernorBlocks verifies the Phase-3 cost governor refuses an
// Execute up front when a session cap is already exceeded (block mode),
// without ever calling the LLM.
func TestExecuteCostGovernorBlocks(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"should not run"}}
	deps := newTestKernel(t, client)

	tr := metrics.NewUsageTracker()
	tr.Record(metrics.RequestRecord{Provider: "test", Model: "m", Cost: 5.0, Success: true})
	deps.Kernel.SetCostGovernor(metrics.NewCostGovernor(tr, metrics.BudgetLimits{
		PerSessionUSD: 1.0, Action: metrics.BudgetActionBlock,
	}))

	_, err := deps.Kernel.Execute(context.Background(), "implement a feature that costs money")
	if err == nil {
		t.Fatal("expected Execute to be blocked by the cost governor")
	}
	if !strings.Contains(err.Error(), "cost limit") {
		t.Errorf("error = %v, want a cost-limit message", err)
	}
	if client.callCount() != 0 {
		t.Errorf("a blocked request must not call the LLM, got %d calls", client.callCount())
	}
}

// TestExecuteCostGovernorWarnProceeds verifies warn mode over the cap still
// runs the request (never silently blocks the user).
func TestExecuteCostGovernorWarnProceeds(t *testing.T) {
	client := &fakeLLMClient{name: "fake", responses: []string{"ran anyway"}}
	deps := newTestKernel(t, client)

	tr := metrics.NewUsageTracker()
	tr.Record(metrics.RequestRecord{Provider: "test", Model: "m", Cost: 5.0, Success: true})
	deps.Kernel.SetCostGovernor(metrics.NewCostGovernor(tr, metrics.BudgetLimits{
		PerSessionUSD: 1.0, Action: metrics.BudgetActionWarn,
	}))

	restore := deps.Kernel.ApplyRequestOverrides("", "", "", "off", "") // general mode, cheap path
	defer restore()

	out, err := deps.Kernel.Execute(context.Background(), "explain how to implement a feature")
	if err != nil {
		t.Fatalf("warn mode should proceed, got error: %v", err)
	}
	if out == "" {
		t.Error("expected a real answer in warn mode")
	}
}
