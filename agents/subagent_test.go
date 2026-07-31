package agents

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/tools"
)

// scriptedClient returns a queued response per call. A response with tool calls
// is expressed by name+args; anything else is plain content.
type scriptedClient struct {
	turns []turn
	calls int32
	err   error
}

type turn struct {
	content string
	tools   []core.ToolCall
}

func toolTurn(name, args string, ids ...string) turn {
	id := "c1"
	if len(ids) > 0 {
		id = ids[0]
	}
	return turn{tools: []core.ToolCall{{ID: id, Type: "function",
		Function: core.FunctionCall{Name: name, Arguments: args}}}}
}

func (s *scriptedClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return s.ChatCompletionStream(ctx, req, nil)
}

func (s *scriptedClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	i := int(atomic.AddInt32(&s.calls, 1)) - 1
	t := turn{content: "done"}
	if i < len(s.turns) {
		t = s.turns[i]
	} else if len(s.turns) > 0 {
		t = s.turns[len(s.turns)-1]
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{
		Message: core.ResponseMessage{Role: "assistant", Content: t.content, ToolCalls: t.tools},
	}}}, nil
}

func (s *scriptedClient) CreateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (s *scriptedClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "scripted", Context: 100000}
}
func (s *scriptedClient) Ping(context.Context) error { return nil }
func (s *scriptedClient) Close() error               { return nil }
func (s *scriptedClient) callCount() int             { return int(atomic.LoadInt32(&s.calls)) }

// countingRegistry records how often each tool actually ran.
func agentTestRegistry(t *testing.T, counts map[string]*int32) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, name := range []string{"read_file", "web_search", "terminal"} {
		n := name
		if counts[n] == nil {
			counts[n] = new(int32)
		}
		readOnly := n != "terminal"
		reg.Register(&tools.ToolEntry{
			Name: n, Description: "test " + n, ReadOnly: readOnly,
			Parameters: tools.MustParseSchema(`{"type":"object","properties":{}}`),
			Handler: func(ctx context.Context, a map[string]interface{}) *tools.ToolResult {
				atomic.AddInt32(counts[n], 1)
				return &tools.ToolResult{Name: n, Success: true, Output: "ok from " + n}
			},
		})
	}
	return reg
}

func spawnAgent(t *testing.T, client core.LLMClient, reg *tools.Registry, cfg core.SubAgentConfig) *SubAgent {
	t.Helper()
	rtr := newScriptedRouter(client)
	f := NewAgentFactory(rtr, reg, nil, nil)
	a, err := f.Spawn(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return a
}

// TestAgentReturnsOnFirstToolFreeAnswer — the ordinary path.
func TestAgentReturnsOnFirstToolFreeAnswer(t *testing.T) {
	counts := map[string]*int32{}
	client := &scriptedClient{turns: []turn{{content: "the answer"}}}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleWorker, Goal: "answer it", MaxTurns: 10})

	res, err := a.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success || res.Output != "the answer" {
		t.Errorf("result = %+v", res)
	}
	if client.callCount() != 1 {
		t.Errorf("made %d calls for a one-shot answer, want 1", client.callCount())
	}
}

// TestExactRepeatGuardStopsAStuckAgent. An agent repeating a byte-identical
// call is not making progress, and the guard exists so it stops rather than
// spending the whole turn budget discovering that.
func TestExactRepeatGuardStopsAStuckAgent(t *testing.T) {
	counts := map[string]*int32{}
	// Always the same call, forever.
	client := &scriptedClient{turns: []turn{toolTurn("read_file", `{"path":"a"}`)}}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleWorker, Goal: "loop forever", MaxTurns: 50})

	res, err := a.Execute(context.Background())
	if err == nil {
		t.Fatal("a stuck agent should surface an error")
	}
	if res.Success {
		t.Error("a stuck agent must not report success")
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Errorf("error should name the problem, got %q", err)
	}
	// It must give up well before MaxTurns rather than burning all 50.
	if client.callCount() > 6 {
		t.Errorf("took %d calls to notice it was stuck", client.callCount())
	}
}

// TestPerToolHardCapForcesAFinalAnswer. The exact-repeat guard only catches
// byte-identical calls, so an agent re-searching with slightly different
// arguments slips past it. The per-tool cap is what makes that converge.
func TestPerToolHardCapForcesAFinalAnswer(t *testing.T) {
	counts := map[string]*int32{}
	var turns []turn
	for i := 0; i < 12; i++ {
		// Different arguments each time — defeats the exact-repeat guard.
		turns = append(turns, toolTurn("web_search", `{"q":"query`+string(rune('a'+i))+`"}`))
	}
	turns = append(turns, turn{content: "final answer"})

	client := &scriptedClient{turns: turns}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleWorker, Goal: "search endlessly", MaxTurns: 30})

	res, _ := a.Execute(context.Background())

	ran := int(atomic.LoadInt32(counts["web_search"]))
	if ran > 6 {
		t.Errorf("web_search ran %d times; the per-tool cap should have stopped it around 5", ran)
	}
	if res == nil {
		t.Fatal("no result")
	}
}

// TestMaxTurnsIsEnforced — the outer bound, when nothing else trips.
func TestMaxTurnsIsEnforced(t *testing.T) {
	counts := map[string]*int32{}
	var turns []turn
	for i := 0; i < 20; i++ {
		turns = append(turns, toolTurn("read_file", `{"path":"f`+string(rune('a'+i))+`"}`))
	}
	client := &scriptedClient{turns: turns}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleWorker, Goal: "never finish", MaxTurns: 3})

	_, err := a.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "max turns") {
		t.Errorf("want a max-turns error, got %v", err)
	}
	if client.callCount() > 4 {
		t.Errorf("ran %d turns with MaxTurns=3", client.callCount())
	}
}

// TestToolResultsReachTheConversation. A tool that ran and whose output never
// made it back into the history is worse than one that failed: the agent
// re-asks and the user pays twice.
func TestToolResultsReachTheConversation(t *testing.T) {
	counts := map[string]*int32{}
	client := &scriptedClient{turns: []turn{
		toolTurn("read_file", `{"path":"x"}`),
		{content: "done"},
	}}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleWorker, Goal: "read then answer", MaxTurns: 10})

	if _, err := a.Execute(context.Background()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var sawToolMsg bool
	for _, m := range a.messages {
		if m.Role == core.RoleTool && strings.Contains(m.ContentString(), "ok from read_file") {
			sawToolMsg = true
		}
	}
	if !sawToolMsg {
		t.Error("the tool ran but its output never entered the conversation")
	}
}

// TestCancellationStopsTheAgent — a cancelled context must not keep spending.
func TestCancellationStopsTheAgent(t *testing.T) {
	counts := map[string]*int32{}
	client := &scriptedClient{turns: []turn{toolTurn("read_file", `{"path":"x"}`)}}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleWorker, Goal: "work", MaxTurns: 20})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := a.Execute(ctx)
	if err == nil {
		t.Fatal("a cancelled context should produce an error")
	}
	if res != nil && res.Success {
		t.Error("a cancelled run must not report success")
	}
	if client.callCount() != 0 {
		t.Errorf("made %d model calls after cancellation", client.callCount())
	}
}

// TestScopedRoleCannotRunAWriteTool end-to-end through Execute, not just
// through the schema filter — this is the injection boundary.
func TestScopedRoleCannotRunAWriteTool(t *testing.T) {
	counts := map[string]*int32{}
	client := &scriptedClient{turns: []turn{
		// The model asks for a tool it was never offered.
		toolTurn("terminal", `{"command":"rm -rf /"}`),
		{content: "gave up"},
	}}
	a := spawnAgent(t, client, agentTestRegistry(t, counts),
		core.SubAgentConfig{Role: core.RoleResearch, Goal: "summarise a page", MaxTurns: 5})

	if _, err := a.Execute(context.Background()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := atomic.LoadInt32(counts["terminal"]); n != 0 {
		t.Fatalf("terminal ran %d time(s) for a research agent", n)
	}
	var refused bool
	for _, m := range a.messages {
		if m.Role == core.RoleTool && strings.Contains(m.ContentString(), "write authority") {
			refused = true
		}
	}
	if !refused {
		t.Error("the refusal never reached the agent, so it cannot adapt")
	}
}

// newScriptedRouter wires a single client across every tier the agent may ask
// for, so a test never has to know which tier a role happens to map to.
func newScriptedRouter(client core.LLMClient) core.ModelRouter {
	return &fixedRouter{client: client}
}

type fixedRouter struct{ client core.LLMClient }

func (f *fixedRouter) Route(core.ModelTier, int, string) (core.LLMClient, string, error) {
	return f.client, "scripted", nil
}
func (f *fixedRouter) Consensus(context.Context, []core.Message, string) (*core.ConsensusResult, error) {
	return nil, nil
}
func (f *fixedRouter) GetMode() core.RoutingMode { return core.RouteSingle }
func (f *fixedRouter) ModelCount() int           { return 1 }
