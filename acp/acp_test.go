package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// execFunc adapts a function to the Executor interface.
type execFunc func(ctx context.Context, cwd, prompt string) (string, error)

func (f execFunc) Execute(ctx context.Context, cwd, prompt string) (string, error) {
	return f(ctx, cwd, prompt)
}

// drive feeds newline-delimited requests through an agent and returns every
// message it wrote, the way an editor would see them.
func drive(t *testing.T, exec Executor, requests ...string) []map[string]interface{} {
	t.Helper()
	var out bytes.Buffer
	agent := NewAgent(exec, &out)
	if err := agent.Serve(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n")); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var msgs []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("agent wrote a line that is not JSON: %q", line)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func echoExec() Executor {
	return execFunc(func(_ context.Context, cwd, prompt string) (string, error) {
		return "cwd=" + cwd + " prompt=" + prompt, nil
	})
}

func TestInitializeAdvertisesProtocolVersion(t *testing.T) {
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)

	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	result, ok := msgs[0]["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result in %v", msgs[0])
	}
	if result["protocolVersion"] != float64(ProtocolVersion) {
		t.Errorf("protocolVersion = %v, want %d", result["protocolVersion"], ProtocolVersion)
	}
	// Claiming a capability we do not implement makes the editor call
	// something that then fails.
	caps, _ := result["agentCapabilities"].(map[string]interface{})
	prompt, _ := caps["promptCapabilities"].(map[string]interface{})
	if prompt["image"] != false || prompt["audio"] != false {
		t.Errorf("advertised a capability we do not implement: %v", prompt)
	}
}

// The full turn an editor performs: open a session, prompt, read the answer.
func TestSessionPromptRoundTrip(t *testing.T) {
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"dc-1","prompt":[{"type":"text","text":"hello"}]}}`)

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want new + update + prompt response: %v", len(msgs), msgs)
	}
	newRes := msgs[0]["result"].(map[string]interface{})
	if newRes["sessionId"] != "dc-1" {
		t.Errorf("sessionId = %v", newRes["sessionId"])
	}

	// The answer arrives as a session/update notification, not in the response.
	if msgs[1]["method"] != "session/update" {
		t.Fatalf("expected session/update, got %v", msgs[1])
	}
	params := msgs[1]["params"].(map[string]interface{})
	update := params["update"].(map[string]interface{})
	content := update["content"].(map[string]interface{})
	if !strings.Contains(content["text"].(string), "cwd=/work") {
		t.Errorf("session cwd not passed to the executor: %v", content)
	}
	if update["sessionUpdate"] != "agent_message_chunk" {
		t.Errorf("wrong update kind: %v", update["sessionUpdate"])
	}

	if got := msgs[2]["result"].(map[string]interface{})["stopReason"]; got != "end_turn" {
		t.Errorf("stopReason = %v, want end_turn", got)
	}
}

// A notification has no id and must never be answered — replying to one
// desynchronises a strict client.
func TestNotificationsAreNeverAnswered(t *testing.T) {
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"dc-1"}}`)
	if len(msgs) != 0 {
		t.Errorf("agent replied to a notification: %v", msgs)
	}
}

// Cancelling before the turn completes must report cancelled, not success.
func TestCancelledPromptReportsCancelled(t *testing.T) {
	var agent *Agent
	exec := execFunc(func(_ context.Context, _, _ string) (string, error) {
		// The client cancels while the turn is running.
		agent.mu.Lock()
		agent.cancelled["dc-1"] = true
		agent.mu.Unlock()
		return "done anyway", nil
	})

	var out bytes.Buffer
	agent = NewAgent(exec, &out)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"dc-1","prompt":[{"type":"text","text":"x"}]}}` + "\n")
	if err := agent.Serve(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"stopReason":"cancelled"`) {
		t.Errorf("expected a cancelled stopReason:\n%s", out.String())
	}
}

func TestUnknownSessionIsRejected(t *testing.T) {
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ghost","prompt":[{"type":"text","text":"hi"}]}}`)
	if len(msgs) != 1 || msgs[0]["error"] == nil {
		t.Fatalf("expected an error for an unknown session, got %v", msgs)
	}
}

func TestUnknownMethodIsRejected(t *testing.T) {
	msgs := drive(t, echoExec(), `{"jsonrpc":"2.0","id":9,"method":"session/teleport"}`)
	if len(msgs) != 1 || msgs[0]["error"] == nil {
		t.Fatalf("expected a method-not-supported error, got %v", msgs)
	}
}

// An executor failure should reach the user as message content; a bare
// protocol error usually surfaces in an editor as a silent red dot.
func TestExecutorErrorBecomesVisibleContent(t *testing.T) {
	failing := execFunc(func(_ context.Context, _, _ string) (string, error) {
		return "", context.DeadlineExceeded
	})
	msgs := drive(t, failing,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"dc-1","prompt":[{"type":"text","text":"x"}]}}`)

	joined, _ := json.Marshal(msgs)
	if !strings.Contains(string(joined), "Error:") {
		t.Errorf("the failure was not surfaced as content: %s", joined)
	}
	last := msgs[len(msgs)-1]["result"].(map[string]interface{})
	if last["stopReason"] != "refusal" {
		t.Errorf("stopReason = %v, want refusal", last["stopReason"])
	}
}

// session/load adopts the client's id so a reopened editor tab keeps talking
// about the same session.
func TestLoadSessionAdoptsClientID(t *testing.T) {
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"from-editor","cwd":"/restored"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"from-editor","prompt":[{"type":"text","text":"hi"}]}}`)

	joined, _ := json.Marshal(msgs)
	if !strings.Contains(string(joined), "cwd=/restored") {
		t.Errorf("loaded session did not keep its working directory: %s", joined)
	}
}

func TestPromptTextFlattensBlocks(t *testing.T) {
	got := promptText([]contentBlock{
		{Type: "text", Text: "explain this"},
		{Type: "resource_link", URI: "file:///a.go", Name: "a.go"},
		{Type: "resource", Text: "inline contents"},
		{Type: "image"}, // unsupported: contributes nothing
		{Type: "text", Text: ""},
	})
	for _, want := range []string{"explain this", "@file:///a.go", "inline contents"} {
		if !strings.Contains(got, want) {
			t.Errorf("promptText missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("empty blocks left blank lines: %q", got)
	}
}

func TestEmptyPromptIsRejected(t *testing.T) {
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"dc-1","prompt":[{"type":"image"}]}}`)
	if msgs[len(msgs)-1]["error"] == nil {
		t.Errorf("a prompt with no text should be an error: %v", msgs)
	}
}

func TestSessionNewRequiresCwd(t *testing.T) {
	msgs := drive(t, echoExec(), `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`)
	if msgs[0]["error"] == nil {
		t.Error("session/new without a cwd should be rejected")
	}
}

// Editors send large prompts with embedded file context; the default scanner
// limit would truncate them into invalid JSON.
func TestLargePromptIsAccepted(t *testing.T) {
	big := strings.Repeat("x", 200_000)
	req, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
		"params": map[string]interface{}{
			"sessionId": "dc-1",
			"prompt":    []map[string]string{{"type": "text", "text": big}},
		},
	})
	msgs := drive(t, echoExec(),
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}`,
		string(req))
	last := msgs[len(msgs)-1]
	if last["error"] != nil {
		t.Errorf("large prompt rejected: %v", last["error"])
	}
}

// Malformed input must be skipped, not kill the connection.
func TestMalformedLineDoesNotEndSession(t *testing.T) {
	msgs := drive(t, echoExec(),
		`not json at all`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if len(msgs) != 1 || msgs[0]["result"] == nil {
		t.Errorf("agent did not recover from a malformed line: %v", msgs)
	}
}
