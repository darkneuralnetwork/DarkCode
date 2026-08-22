package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

// geminiToolCallResponse is a real response from
// generativelanguage.googleapis.com/v1beta/openai, captured verbatim.
//
// The shape is the whole point: the signature is on the TOOL CALL, under
// extra_content.google, and NOT inside the function object. An earlier version
// of this client declared the field inside FunctionCall, so it parsed nothing,
// echoed nothing back, and Gemini refused the following request with
// "400 INVALID_ARGUMENT — Function call is missing a thought_signature". The
// agentic loop died on iteration 2 of every tool-using task.
const geminiToolCallResponse = `{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "ercDxfn4",
        "type": "function",
        "function": {"name": "search_files", "arguments": "{\"q\":\"cat\"}"},
        "extra_content": {"google": {"thought_signature": "EjQKMgERTTIPLKnelJEyURkOouODOi5V"}}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// TestGeminiThoughtSignatureIsParsed — if it is not captured there is nothing
// to send back, and the next turn fails.
func TestGeminiThoughtSignatureIsParsed(t *testing.T) {
	var resp core.CompletionResponse
	if err := json.Unmarshal([]byte(geminiToolCallResponse), &resp); err != nil {
		t.Fatal(err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("parsed %d tool calls, want 1", len(calls))
	}
	if got := calls[0].ExtraContent.Signature(); got != "EjQKMgERTTIPLKnelJEyURkOouODOi5V" {
		t.Errorf("signature = %q, want the one on the wire", got)
	}
}

// TestThoughtSignatureSurvivesTheRoundTrip. Parsing it is half the job: the
// loop appends the assistant message to the history and sends it back, so the
// signature has to serialise into the request in the same place Gemini put it.
func TestThoughtSignatureSurvivesTheRoundTrip(t *testing.T) {
	var resp core.CompletionResponse
	if err := json.Unmarshal([]byte(geminiToolCallResponse), &resp); err != nil {
		t.Fatal(err)
	}

	// Exactly what loop.go does with the response.
	msg := core.Message{
		Role:      core.RoleAssistant,
		ToolCalls: resp.Choices[0].Message.ToolCalls,
	}
	out, err := json.Marshal(core.CompletionRequest{Messages: []core.Message{msg}})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)

	if !strings.Contains(body, `"extra_content"`) {
		t.Fatalf("the request carries no extra_content:\n%s", body)
	}
	if !strings.Contains(body, "EjQKMgERTTIPLKnelJEyURkOouODOi5V") {
		t.Fatalf("the signature was dropped on the way out:\n%s", body)
	}
	// It must be nested under google, not flattened — Gemini reads that path.
	if !strings.Contains(body, `"google":{"thought_signature":`) {
		t.Errorf("extra_content is not in the shape Gemini sends:\n%s", body)
	}
}

// TestNoExtraContentForOtherProviders. The field is sent to every provider on
// the same request path, so an empty one must vanish rather than reach an
// endpoint that would reject an unknown key.
func TestNoExtraContentForOtherProviders(t *testing.T) {
	msg := core.Message{
		Role: core.RoleAssistant,
		ToolCalls: []core.ToolCall{{
			ID: "call_1", Type: "function",
			Function: core.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
		}},
	}
	out, err := json.Marshal(core.CompletionRequest{Messages: []core.Message{msg}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "extra_content") {
		t.Errorf("a tool call with no provider state still sent extra_content:\n%s", out)
	}
}

// TestStreamedSignatureIsNotConcatenated.
//
// Arguments arrive as text deltas and are joined; the signature is an opaque
// blob that arrives whole. Joining two copies would corrupt it into something
// Gemini rejects, which is the same failure with a harder cause to see.
func TestStreamedSignatureIsNotConcatenated(t *testing.T) {
	acc := &core.ToolCall{}
	deltas := []*core.ToolCallExtra{
		nil,
		core.WithSignature("SIG"),
		core.WithSignature("SIG"), // provider repeats it on a later chunk
	}
	for _, d := range deltas {
		if acc.ExtraContent.Signature() == "" && d.Signature() != "" {
			acc.ExtraContent = d
		}
	}
	if got := acc.ExtraContent.Signature(); got != "SIG" {
		t.Errorf("accumulated signature = %q, want it kept whole", got)
	}
}

// TestSignatureHelpersAreNilSafe — every call site would otherwise repeat two
// pointer checks, and one of them would be forgotten.
func TestSignatureHelpersAreNilSafe(t *testing.T) {
	var nilExtra *core.ToolCallExtra
	if nilExtra.Signature() != "" {
		t.Error("a nil extra reported a signature")
	}
	if (&core.ToolCallExtra{}).Signature() != "" {
		t.Error("an extra with no google branch reported a signature")
	}
	if core.WithSignature("") != nil {
		t.Error("an empty signature produced a non-nil extra, which would serialise")
	}
}
