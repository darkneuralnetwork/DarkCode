package compression

import (
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// Reproduces the reported Gemini 400: "GenerateContentRequest.contents: contents
// is not specified". Gemini folds system messages into systemInstruction, so a
// message list containing ONLY system messages sends an empty contents array.
func TestFitToWindowNeverStripsAllConversation(t *testing.T) {
	huge := strings.Repeat("drwxr-xr-x 2 kali kali 4096 some/long/path/entry\n", 4000)

	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "You are a worker agent."},
		{Role: core.RoleUser, Content: "create a directory called myip"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c1",
			Function: core.FunctionCall{Name: "terminal", Arguments: `{"command":"mkdir myip"}`}}}},
		{Role: core.RoleTool, ToolCallID: "c1", Name: "terminal",
			Content: "mkdir: cannot create directory 'myip': Read-only file system"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c2",
			Function: core.FunctionCall{Name: "list_dir", Arguments: `{"path":"."}`}}}},
		{Role: core.RoleTool, ToolCallID: "c2", Name: "list_dir", Content: huge},
	}

	out := FitToWindow(msgs, 8000, 3000)

	t.Logf("fitted to %d message(s):", len(out))
	for _, m := range out {
		t.Logf("  role=%-9s tokens=%d toolcalls=%d", m.Role,
			EstimateStringTokens(m.ContentString()), len(m.ToolCalls))
	}

	conversational := 0
	for _, m := range out {
		if m.Role != core.RoleSystem {
			conversational++
		}
	}
	if conversational == 0 {
		t.Fatalf("every non-system message was stripped — Gemini receives an empty contents array and returns 400")
	}
}

// TestFitToWindowKeepsMultiToolExchangeIntact. One assistant turn can request
// several tools at once; keeping the assistant and only some of its responses
// leaves calls unanswered, which Gemini rejects just as firmly as an orphan.
func TestFitToWindowKeepsMultiToolExchangeIntact(t *testing.T) {
	big := strings.Repeat("output line\n", 3000)
	msgs := []core.Message{
		{Role: core.RoleSystem, Content: "sys"},
		{Role: core.RoleUser, Content: strings.Repeat("old context. ", 2000)},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "a", Function: core.FunctionCall{Name: "read_file"}},
			{ID: "b", Function: core.FunctionCall{Name: "list_dir"}},
		}},
		{Role: core.RoleTool, ToolCallID: "a", Name: "read_file", Content: big},
		{Role: core.RoleTool, ToolCallID: "b", Name: "list_dir", Content: "short"},
	}

	out := FitToWindow(msgs, 8000, 3000)

	asked := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			asked[tc.ID] = true
		}
		if m.Role == core.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for id := range asked {
		if !answered[id] {
			t.Errorf("tool call %q survived with no response — the request is malformed", id)
		}
	}
	for id := range answered {
		if !asked[id] {
			t.Errorf("tool response %q survived with no call — the request is malformed", id)
		}
	}
}

// TestFitToWindowAlwaysLeavesSomethingToSay is the post-condition itself,
// independent of how the anchors happen to be chosen.
func TestFitToWindowAlwaysLeavesSomethingToSay(t *testing.T) {
	cases := [][]core.Message{
		{{Role: core.RoleSystem, Content: strings.Repeat("s", 40000)},
			{Role: core.RoleUser, Content: strings.Repeat("u", 40000)}},
		{{Role: core.RoleSystem, Content: "sys"},
			{Role: core.RoleUser, Content: "do it"},
			{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "x", Function: core.FunctionCall{Name: "t"}}}},
			{Role: core.RoleTool, ToolCallID: "x", Content: strings.Repeat("z", 200000)}},
	}
	for i, msgs := range cases {
		out := FitToWindow(msgs, 4000, 2000)
		found := false
		for _, m := range out {
			if m.Role != core.RoleSystem {
				found = true
			}
		}
		if !found {
			t.Errorf("case %d: fitted request has no conversational content", i)
		}
	}
}
