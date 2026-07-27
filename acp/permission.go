package acp

// permission.go — asking the editor for approval.
//
// Everywhere else DarkCode runs, a dangerous tool call reaches a human: the
// CLI prints a prompt, the GUI raises a dialog. Under ACP the editor owns the
// conversation surface, so the agent has no window of its own to put a
// question in. The first cut auto-approved instead, which quietly made the
// editor the least guarded way to run the agent — deny rules still held, but
// blast-radius escalation and every high-risk action passed unchallenged.
//
// ACP answers this with session/request_permission: the agent asks, the editor
// renders the choice in its own UI, the user decides. That is the flow here.
//
// Two properties are deliberate. The request travels agent → client, which is
// the reverse of every other message in this package and is why Serve has to
// route replies. And every path that does not end in an explicit approval —
// timeout, cancellation, a malformed answer, a client that ignores the request
// entirely — denies. An editor that does not implement the method must not
// become a way to run unapproved commands.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PermissionOption is one choice offered to the user.
type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"` // allow_once | allow_always | reject_once
}

// Standard option ids. The editor echoes one of these back.
const (
	OptAllowOnce   = "allow-once"
	OptAllowAlways = "allow-always"
	OptReject      = "reject-once"
)

// DefaultPermissionOptions is the choice set offered for a tool call.
func DefaultPermissionOptions() []PermissionOption {
	return []PermissionOption{
		{ID: OptAllowOnce, Name: "Allow once", Kind: "allow_once"},
		{ID: OptAllowAlways, Name: "Allow for this session", Kind: "allow_always"},
		{ID: OptReject, Name: "Reject", Kind: "reject_once"},
	}
}

// PermissionRequest describes the action needing approval, in the shape ACP
// expects. Title is what the user reads, so it must say what will happen.
type PermissionRequest struct {
	Title   string
	Kind    string // edit | execute | read | fetch | other
	Content string // the diff or command being approved, when there is one
	Options []PermissionOption
}

// permissionTimeout bounds the wait for a decision. An editor that never
// answers must not strand the agent, and the denial that follows is the safe
// outcome. It is generous because a human is reading a diff.
const permissionTimeout = 10 * time.Minute

// ErrNoSession reports that no prompt is running, so there is no editor UI to
// ask. Callers must treat it as a denial.
var ErrNoSession = fmt.Errorf("acp: no active session to request permission from")

// RequestPermission asks the editor to approve an action and blocks until the
// user decides. It returns the chosen option id, or an error for every outcome
// that is not an explicit choice — the caller denies on any error.
func (a *Agent) RequestPermission(ctx context.Context, req PermissionRequest) (string, error) {
	a.mu.Lock()
	session := a.active
	a.mu.Unlock()
	if session == "" {
		return "", ErrNoSession
	}

	if len(req.Options) == 0 {
		req.Options = DefaultPermissionOptions()
	}
	toolCall := map[string]interface{}{
		"toolCallId": fmt.Sprintf("dc-perm-%d", a.nextID.Add(1)),
		"title":      req.Title,
		"kind":       nonBlank(req.Kind, "other"),
		"status":     "pending",
	}
	if req.Content != "" {
		toolCall["content"] = []map[string]interface{}{
			{"type": "content", "content": map[string]interface{}{"type": "text", "text": req.Content}},
		}
	}

	reply, err := a.call(ctx, "session/request_permission", map[string]interface{}{
		"sessionId": session,
		"toolCall":  toolCall,
		"options":   req.Options,
	})
	if err != nil {
		return "", err
	}
	if reply.Err != nil {
		return "", fmt.Errorf("acp: client refused the permission request: %s", reply.Err.Message)
	}

	// The outcome is nested: {"outcome": {"outcome": "selected", "optionId": …}}.
	var out struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		return "", fmt.Errorf("acp: unreadable permission outcome: %w", err)
	}
	if out.Outcome.Outcome != "selected" || out.Outcome.OptionID == "" {
		// "cancelled" lands here, and so does anything unrecognised.
		return "", fmt.Errorf("acp: permission not granted (%s)", nonBlank(out.Outcome.Outcome, "no outcome"))
	}
	return out.Outcome.OptionID, nil
}

// call sends a request to the client and waits for the matching reply.
func (a *Agent) call(ctx context.Context, method string, params interface{}) (rpcReply, error) {
	id := a.nextID.Add(1)
	ch := make(chan rpcReply, 1) // buffered: deliver must never block on a gone waiter

	a.pendingMu.Lock()
	a.pending[id] = ch
	a.pendingMu.Unlock()

	// Whatever happens, stop holding a slot for this id.
	defer func() {
		a.pendingMu.Lock()
		delete(a.pending, id)
		a.pendingMu.Unlock()
	}()

	a.send(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})

	timeout := time.NewTimer(permissionTimeout)
	defer timeout.Stop()

	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		return rpcReply{}, ctx.Err()
	case <-timeout.C:
		return rpcReply{}, fmt.Errorf("acp: no answer to %s within %s", method, permissionTimeout)
	}
}

func nonBlank(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
