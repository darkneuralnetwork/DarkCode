package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClient is an editor: it reads what the agent writes and answers any
// session/request_permission with a scripted outcome. The existing drive()
// helper cannot do this — it plays a fixed script and never reacts — and
// reacting is the entire point of the permission flow.
type fakeClient struct {
	// reply builds the answer to a permission request. Returning nil means
	// "ignore it", which is how an editor without the capability behaves.
	reply func(id json.RawMessage, params json.RawMessage) interface{}

	mu       sync.Mutex
	requests []json.RawMessage // params of every permission request seen
	toAgent  io.Writer
}

func (c *fakeClient) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Method != "session/request_permission" {
			continue
		}
		c.mu.Lock()
		c.requests = append(c.requests, msg.Params)
		c.mu.Unlock()

		if c.reply == nil {
			continue
		}
		if answer := c.reply(msg.ID, msg.Params); answer != nil {
			body, _ := json.Marshal(answer)
			_, _ = c.toAgent.Write(append(body, '\n'))
		}
	}
	return len(p), nil
}

// runWithClient starts an agent whose executor calls ask(), with a client that
// answers permission requests via reply. It returns what ask() produced.
func runWithClient(t *testing.T, reply func(id, params json.RawMessage) interface{},
	ask func(a *Agent) (string, error)) (string, error, *fakeClient) {
	t.Helper()

	pr, pw := io.Pipe()
	client := &fakeClient{reply: reply, toAgent: pw}

	var (
		mu     sync.Mutex
		got    string
		gotErr error
		asked  bool
	)
	// The executor stands in for a tool call reaching the permission gate.
	// agent is captured by the closure, so it is built in two steps.
	agent := &Agent{out: client, sessions: map[string]string{},
		cancelled: map[string]bool{}, pending: map[int64]chan rpcReply{}}
	agent.exec = execFunc(func(_ context.Context, _, _ string) (string, error) {
		g, err := ask(agent)
		mu.Lock()
		got, gotErr, asked = g, err, true
		mu.Unlock()
		return "done", nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.Serve(context.Background(), pr)
	}()

	_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}` + "\n"))
	_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"dc-1","prompt":[{"type":"text","text":"go"}]}}` + "\n"))

	// Wait for the turn to reach its verdict, then close the stream so Serve
	// returns.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		finished := asked
		mu.Unlock()
		if finished {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the turn did not finish")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	_ = pw.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	return got, gotErr, client
}

// selected builds the "user picked an option" reply.
func selected(id json.RawMessage, optionID string) interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "selected", "optionId": optionID},
		},
	}
}

func TestRequestPermissionReturnsTheChosenOption(t *testing.T) {
	for _, want := range []string{OptAllowOnce, OptAllowAlways, OptReject} {
		got, err, _ := runWithClient(t,
			func(id, _ json.RawMessage) interface{} { return selected(id, want) },
			func(a *Agent) (string, error) {
				return a.RequestPermission(context.Background(), PermissionRequest{Title: "Run tests"})
			})
		if err != nil {
			t.Fatalf("RequestPermission: %v", err)
		}
		if got != want {
			t.Errorf("option = %q, want %q", got, want)
		}
	}
}

// The security property: anything that is not an explicit selection must not
// read as approval.
func TestRequestPermissionFailsClosed(t *testing.T) {
	cases := map[string]func(id, params json.RawMessage) interface{}{
		"editor ignores the request": nil,
		"user cancelled": func(id, _ json.RawMessage) interface{} {
			return map[string]interface{}{"jsonrpc": "2.0", "id": id,
				"result": map[string]interface{}{"outcome": map[string]interface{}{"outcome": "cancelled"}}}
		},
		"client returned an error": func(id, _ json.RawMessage) interface{} {
			return map[string]interface{}{"jsonrpc": "2.0", "id": id,
				"error": map[string]interface{}{"code": -32601, "message": "method not found"}}
		},
		"unreadable outcome": func(id, _ json.RawMessage) interface{} {
			return map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "not an object"}
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			// A client that never answers must not hang the agent forever, but
			// waiting out the real timeout would stall the suite; a cancelled
			// context stands in for it.
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()

			got, err, _ := runWithClient(t, reply, func(a *Agent) (string, error) {
				return a.RequestPermission(ctx, PermissionRequest{Title: "rm -rf /"})
			})
			if err == nil {
				t.Fatalf("%s was accepted as approval (option %q)", name, got)
			}
			if got != "" {
				t.Errorf("an option was returned alongside the error: %q", got)
			}
		})
	}
}

// Without a running prompt there is no editor UI to ask, which must deny
// rather than proceed unapproved.
func TestRequestPermissionWithoutASessionDenies(t *testing.T) {
	agent := NewAgent(echoExec(), io.Discard)
	if _, err := agent.RequestPermission(context.Background(),
		PermissionRequest{Title: "delete everything"}); err == nil {
		t.Fatal("a permission request with no active session was granted")
	}
}

// The request must carry enough for the editor to render a real choice.
func TestPermissionRequestCarriesTitleAndOptions(t *testing.T) {
	_, _, client := runWithClient(t,
		func(id, _ json.RawMessage) interface{} { return selected(id, OptAllowOnce) },
		func(a *Agent) (string, error) {
			return a.RequestPermission(context.Background(), PermissionRequest{
				Title: "Write config.yaml", Kind: "edit", Content: "+ debug: true",
			})
		})

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) != 1 {
		t.Fatalf("got %d permission requests, want 1", len(client.requests))
	}
	var p struct {
		SessionID string             `json:"sessionId"`
		ToolCall  map[string]any     `json:"toolCall"`
		Options   []PermissionOption `json:"options"`
	}
	if err := json.Unmarshal(client.requests[0], &p); err != nil {
		t.Fatalf("unreadable request: %v", err)
	}
	if p.SessionID == "" {
		t.Error("no session id: the editor cannot tell which tab is asking")
	}
	if p.ToolCall["title"] != "Write config.yaml" {
		t.Errorf("title = %v, want the action described", p.ToolCall["title"])
	}
	if p.ToolCall["kind"] != "edit" {
		t.Errorf("kind = %v, want edit", p.ToolCall["kind"])
	}
	if len(p.Options) != 3 {
		t.Errorf("got %d options, want allow-once / allow-always / reject", len(p.Options))
	}
}

// session/cancel used to be unreachable while a prompt ran, because dispatch
// was sequential: the loop could not read the cancel until the prompt it meant
// to interrupt had already returned.
func TestCancelIsProcessedWhileAPromptRuns(t *testing.T) {
	pr, pw := io.Pipe()
	started := make(chan struct{})
	release := make(chan struct{})

	agent := NewAgent(execFunc(func(_ context.Context, _, _ string) (string, error) {
		close(started)
		<-release // hold the turn open while the cancel arrives
		return "done", nil
	}), io.Discard)

	done := make(chan struct{})
	go func() { defer close(done); _ = agent.Serve(context.Background(), pr) }()

	_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}` + "\n"))
	_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"dc-1","prompt":[{"type":"text","text":"go"}]}}` + "\n"))

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never started")
	}

	// This line can only be read if the loop is not blocked on the prompt.
	_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"dc-1"}}` + "\n"))

	deadline := time.After(5 * time.Second)
	for {
		agent.mu.Lock()
		cancelled := agent.cancelled["dc-1"]
		agent.mu.Unlock()
		if cancelled {
			break
		}
		select {
		case <-deadline:
			t.Fatal("session/cancel was not processed while the prompt was running")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	close(release)
	_ = pw.Close()
	<-done
}
