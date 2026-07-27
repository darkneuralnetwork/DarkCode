// Package acp serves DarkCode over the Agent Client Protocol, so editors that
// speak ACP — Zed, and the VS Code and JetBrains bridges — can drive it
// without any editor-specific code.
//
// One protocol, every editor, is the whole point. The alternative is an
// extension per editor, each with its own release cycle and its own bugs.
//
// Wire format is newline-delimited JSON-RPC 2.0 over stdio — one JSON object
// per line, no Content-Length framing (that is LSP and DAP; ACP differs, and
// assuming otherwise produces a silent hang).
//
// The editor is the client and DarkCode is the agent: the editor calls
// initialize, session/new and session/prompt on us, and we call back with
// session/update notifications as the answer streams.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the ACP revision this implements.
const ProtocolVersion = 1

// Executor is the agent behind the protocol. Implemented by the orchestrator
// kernel; kept as an interface so this package neither imports it nor needs a
// running model to be tested.
type Executor interface {
	// Execute answers one prompt within a session rooted at cwd.
	Execute(ctx context.Context, cwd, prompt string) (string, error)
}

// Agent serves one ACP connection.
type Agent struct {
	exec Executor
	out  io.Writer

	mu       sync.Mutex
	sessions map[string]string // session id → working directory
	nextID   atomic.Int64
	// cancelled records sessions the client asked to stop, so an in-flight
	// prompt can report that it was cancelled rather than pretending it
	// finished normally.
	cancelled map[string]bool
}

// NewAgent builds an agent that writes protocol messages to out.
func NewAgent(exec Executor, out io.Writer) *Agent {
	return &Agent{
		exec: exec, out: out,
		sessions:  map[string]string{},
		cancelled: map[string]bool{},
	}
}

// rpcRequest is an incoming JSON-RPC message. A nil ID marks a notification,
// which must never be answered.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests until the stream ends. It returns nil on a clean EOF,
// which is how an editor closing the connection looks.
func (a *Agent) Serve(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	// An editor can legitimately send a large prompt with embedded file
	// context; the default 64 KiB token limit would truncate it into invalid
	// JSON.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// No id is recoverable here, so there is nobody to tell.
			continue
		}
		a.dispatch(ctx, req)
	}
	return scanner.Err()
}

// dispatch routes one request and replies when it has an id.
func (a *Agent) dispatch(ctx context.Context, req rpcRequest) {
	result, err := a.handle(ctx, req.Method, req.Params)
	if req.ID == nil {
		return // notification: never answered, even on error
	}
	if err != nil {
		a.send(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"error": rpcError{Code: -32603, Message: err.Error()},
		})
		return
	}
	a.send(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// handle implements the agent side of ACP.
func (a *Agent) handle(ctx context.Context, method string, params json.RawMessage) (interface{}, error) {
	switch method {
	case "initialize":
		// The response advertises what this agent can do. Claiming a
		// capability we do not implement makes the editor call something that
		// then fails, so this lists only what is real.
		return map[string]interface{}{
			"protocolVersion": ProtocolVersion,
			"agentCapabilities": map[string]interface{}{
				"loadSession": true,
				"promptCapabilities": map[string]interface{}{
					"image": false, "audio": false, "embeddedContext": true,
				},
			},
			// No auth: DarkCode runs locally as the user.
			"authMethods": []interface{}{},
		}, nil

	case "authenticate":
		return map[string]interface{}{}, nil

	case "session/new":
		var p struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Cwd == "" {
			return nil, fmt.Errorf("session/new requires an absolute cwd")
		}
		id := fmt.Sprintf("dc-%d", a.nextID.Add(1))
		a.mu.Lock()
		a.sessions[id] = p.Cwd
		a.mu.Unlock()
		return map[string]interface{}{"sessionId": id}, nil

	case "session/load":
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		_ = json.Unmarshal(params, &p)
		if p.SessionID == "" {
			return nil, fmt.Errorf("session/load requires a sessionId")
		}
		// Adopt the id the client remembers rather than minting a new one, so
		// a reopened editor tab keeps talking about the same session.
		a.mu.Lock()
		a.sessions[p.SessionID] = p.Cwd
		delete(a.cancelled, p.SessionID)
		a.mu.Unlock()
		return map[string]interface{}{}, nil

	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &p)
		a.mu.Lock()
		a.cancelled[p.SessionID] = true
		a.mu.Unlock()
		return nil, nil // a notification; dispatch drops the result

	case "session/prompt":
		return a.prompt(ctx, params)

	case "session/set_mode", "session/set_model":
		// Accepted so an editor offering the control is not broken by it;
		// DarkCode's mode and model are configured on its own side.
		return map[string]interface{}{}, nil
	}
	return nil, fmt.Errorf("method not supported: %s", method)
}

// contentBlock is one piece of a prompt. Only text and resource links are
// required of an agent; the rest are opt-in capabilities this does not claim.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URI  string `json:"uri,omitempty"`
	Name string `json:"name,omitempty"`
}

// prompt runs one turn and streams the answer back as session/update.
func (a *Agent) prompt(ctx context.Context, params json.RawMessage) (interface{}, error) {
	var p struct {
		SessionID string         `json:"sessionId"`
		Prompt    []contentBlock `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid prompt: %w", err)
	}

	a.mu.Lock()
	cwd, known := a.sessions[p.SessionID]
	delete(a.cancelled, p.SessionID) // a new turn clears a previous cancel
	a.mu.Unlock()
	if !known {
		return nil, fmt.Errorf("unknown session %q — call session/new first", p.SessionID)
	}

	text := promptText(p.Prompt)
	if text == "" {
		return nil, fmt.Errorf("prompt contained no text")
	}

	answer, err := a.exec.Execute(ctx, cwd, text)
	if err != nil {
		// Report the failure as the turn's content: an editor shows the user
		// the message, whereas a protocol error usually surfaces as a silent
		// red dot.
		a.update(p.SessionID, "agent_message_chunk", "Error: "+err.Error())
		return map[string]interface{}{"stopReason": "refusal"}, nil
	}

	a.mu.Lock()
	cancelled := a.cancelled[p.SessionID]
	a.mu.Unlock()
	if cancelled {
		return map[string]interface{}{"stopReason": "cancelled"}, nil
	}

	a.update(p.SessionID, "agent_message_chunk", answer)
	return map[string]interface{}{"stopReason": "end_turn"}, nil
}

// promptText flattens the content blocks an editor sends into one prompt.
// Resource links carry a path the user @-mentioned, which is meaningful even
// though the contents are not inlined.
func promptText(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "resource_link":
			if b.URI != "" {
				parts = append(parts, "@"+b.URI)
			}
		case "resource":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return joinNonEmpty(parts, "\n")
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

// update sends a session/update notification carrying a chunk of the answer.
func (a *Agent) update(sessionID, kind, text string) {
	a.send(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]interface{}{
			"sessionId": sessionID,
			"update": map[string]interface{}{
				"sessionUpdate": kind,
				"content":       map[string]interface{}{"type": "text", "text": text},
			},
		},
	})
}

// send writes one newline-delimited message. Writes are serialised because
// notifications and responses can race.
func (a *Agent) send(msg interface{}) {
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.out.Write(append(body, '\n'))
}
