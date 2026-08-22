package compression

import (
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

func bigMsg(role core.Role, n int) core.Message {
	return core.Message{Role: role, Content: strings.Repeat("word ", n)}
}

// The core contract: whatever comes in, the output fits window−reserve.
func TestFitToWindow_AlwaysFitsWindow(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "system prompt here"},
		bigMsg(core.RoleUser, 2000),
		bigMsg(core.RoleAssistant, 2000),
		bigMsg(core.RoleUser, 2000),
		{Role: core.RoleUser, Content: "the current question"},
	}
	window, reserve := 1000, 200
	out := FitToWindow(msgs, window, reserve)
	if got := EstimateTokens(out); got > window-reserve {
		t.Errorf("fitted messages still over budget: %d > %d", got, window-reserve)
	}
}

// Invariant: the system prompt and the last (current) turn survive.
func TestFitToWindow_PreservesSystemAndLastTurn(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "SYSTEM-ANCHOR"},
		bigMsg(core.RoleUser, 3000),
		bigMsg(core.RoleAssistant, 3000),
		{Role: core.RoleUser, Content: "LAST-TURN-ANCHOR"},
	}
	out := FitToWindow(msgs, 500, 100)
	if len(out) == 0 || !strings.Contains(out[0].ContentString(), "SYSTEM-ANCHOR") {
		t.Errorf("system prompt was dropped: %+v", out)
	}
	last := out[len(out)-1].ContentString()
	if !strings.Contains(last, "LAST-TURN-ANCHOR") {
		t.Errorf("last turn was dropped: %q", last)
	}
}

// A slice already within budget is returned untouched.
func TestFitToWindow_NoOpWhenWithinBudget(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "hi"},
		{Role: core.RoleUser, Content: "short question"},
	}
	out := FitToWindow(msgs, 100000, 1000)
	if len(out) != len(msgs) {
		t.Errorf("within-budget slice was modified: got %d msgs, want %d", len(out), len(msgs))
	}
}

// A zero/unknown window must not destroy context (safe on a bad signal).
func TestFitToWindow_UnknownWindowReturnsInput(t *testing.T) {
	msgs := []core.Message{bigMsg(core.RoleUser, 5000)}
	out := FitToWindow(msgs, 0, 100)
	if len(out) != 1 {
		t.Errorf("window=0 must return input unchanged, got %d msgs", len(out))
	}
}

// Phase 2: when system+last alone blow the budget, the big one is
// middle-truncated rather than the call being impossible.
func TestFitToWindow_TruncatesWhenAnchorsExceedBudget(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "sys"},
		bigMsg(core.RoleUser, 10000), // last turn alone is huge
	}
	out := FitToWindow(msgs, 800, 100)
	if got := EstimateTokens(out); got > 800 {
		t.Errorf("anchors not truncated to budget: %d > 800", got)
	}
	if len(out) != 2 {
		t.Errorf("expected both anchors retained (truncated), got %d msgs", len(out))
	}
}

// FitClient reads the client's effective window via ModelInfo().Context.
type fakeWindowClient struct {
	core.LLMClient
	window int
}

func (f fakeWindowClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "fake", Context: f.window}
}

func TestFitClient_UsesClientEffectiveWindow(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "sys"},
		bigMsg(core.RoleUser, 5000),
		{Role: core.RoleUser, Content: "q"},
	}
	// Small effective window (e.g. a local model at n_ctx/np) must force a trim.
	out := FitClient(msgs, fakeWindowClient{window: 2048}, 0, 0)
	if EstimateTokens(out) > 2048 {
		t.Errorf("FitClient did not respect the client's 2048 window: %d", EstimateTokens(out))
	}
}

func TestFitClient_FallsBackToCfgContextLength(t *testing.T) {
	msgs := []core.Message{bigMsg(core.RoleUser, 5000), {Role: core.RoleUser, Content: "q"}}
	// window=0 (client can't report) → cfgContextLength is the budget source.
	out := FitClient(msgs, fakeWindowClient{window: 0}, 3000, 0)
	if EstimateTokens(out) > 3000 {
		t.Errorf("FitClient did not fall back to cfgContextLength=3000: %d", EstimateTokens(out))
	}
}

// Shedding must never split a tool exchange. An orphaned tool response is
// rejected outright by Gemini's OpenAI-compatible endpoint ("function call
// turn must come immediately after a user turn or a function response turn"),
// and the failure lands at the API boundary with nothing pointing back at the
// trimmer that caused it.
func TestFitToWindow_NeverOrphansToolMessages(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "system"},
		bigMsg(core.RoleUser, 1500),
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "call-1", Function: core.FunctionCall{Name: "read_file", Arguments: "{}"}},
		}},
		{Role: core.RoleTool, ToolCallID: "call-1", Name: "read_file", Content: strings.Repeat("x", 4000)},
		bigMsg(core.RoleAssistant, 1500),
		{Role: core.RoleUser, Content: "the current question"},
	}

	out := FitToWindow(msgs, 800, 100)

	asked := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			asked[tc.ID] = true
		}
	}
	answered := map[string]bool{}
	for _, m := range out {
		if m.Role == core.RoleTool {
			if m.ToolCallID == "" {
				t.Error("a tool message survived with no tool_call_id")
				continue
			}
			answered[m.ToolCallID] = true
			if !asked[m.ToolCallID] {
				t.Errorf("orphaned tool response for %q — the call that asked for it was shed", m.ToolCallID)
			}
		}
	}
	for id := range asked {
		if !answered[id] {
			t.Errorf("tool call %q survived with no response — the call is left hanging", id)
		}
	}
}

// Repair must not throw away an assistant's prose just because its tool calls
// lost their answers.
func TestRepairToolPairsKeepsContentWhenDroppingCalls(t *testing.T) {
	got := repairToolPairs([]core.Message{
		{Role: core.RoleAssistant, Content: "here is my reasoning", ToolCalls: []core.ToolCall{
			{ID: "gone", Function: core.FunctionCall{Name: "f"}},
		}},
	})
	if len(got) != 1 {
		t.Fatalf("got %d messages, want the assistant kept for its content", len(got))
	}
	if len(got[0].ToolCalls) != 0 {
		t.Error("an unanswered tool call was kept")
	}
	if got[0].ContentString() != "here is my reasoning" {
		t.Errorf("the assistant's content was lost: %q", got[0].ContentString())
	}
}

// A complete exchange must pass through untouched.
func TestRepairToolPairsLeavesCompleteExchangesAlone(t *testing.T) {
	in := []core.Message{
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "a", Function: core.FunctionCall{Name: "f"}}}},
		{Role: core.RoleTool, ToolCallID: "a", Name: "f", Content: "result"},
	}
	if got := repairToolPairs(in); len(got) != 2 {
		t.Errorf("a complete exchange was altered: %d of 2 kept", len(got))
	}
}
