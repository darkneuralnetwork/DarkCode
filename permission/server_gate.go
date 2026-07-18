package permission

// ============================================================================
// SERVER APPROVER
//
// An Approver implementation for web/GUI mode. When a dangerous tool call
// needs approval, the ServerApprover:
//
//   1. registers a pending approval request with a unique id
//   2. notifies the UI layer via an OnRequest callback (which the server
//      wires to an SSE EventApproval broadcast → browser popup)
//   3. blocks the calling goroutine (the tool-dispatch path) until the UI
//      submits a decision via Resolve(), or the timeout elapses
//
// This lets the web UI pop up a permission dialog and wait for the user's
// choice while the orchestrator is mid-execution.
// ============================================================================

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// PendingApproval is the JSON-friendly representation of an unresolved
// approval request, returned by the /api/approvals endpoint.
type PendingApproval struct {
	ID      string    `json:"id"`
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"`
	Preview string    `json:"preview"`
	Risk    string    `json:"risk"`
	Created time.Time `json:"created"`
}

// RequestFn is invoked when a new approval is needed. The server wires this
// to emit an SSE event so the browser can show a popup. It receives the
// approval id (to echo back in the resolve call) and the full request.
type RequestFn func(id string, req ApprovalRequest)

// ServerApprover is an Approver that defers to a remote UI (the web frontend).
// It blocks until the UI resolves the request, or until the timeout expires.
type ServerApprover struct {
	// counter is accessed via atomic.AddUint64 — first field for 8-byte
	// alignment on 32-bit platforms (386/arm), where a misaligned 64-bit
	// atomic panics. See memory/writer.go for the same fix.
	counter uint64

	mu        sync.Mutex
	pending   map[string]*pendingEntry
	onRequest RequestFn
	timeout   time.Duration
}

type pendingEntry struct {
	request ApprovalRequest
	created time.Time
	ch      chan Verdict
}

// NewServerApprover creates a server-backed approver with a 5-minute default
// timeout per request.
func NewServerApprover() *ServerApprover {
	return &ServerApprover{
		pending: make(map[string]*pendingEntry),
		timeout: 5 * time.Minute,
	}
}

// OnRequest registers the callback fired when a new approval is needed.
func (s *ServerApprover) OnRequest(fn RequestFn) {
	s.mu.Lock()
	s.onRequest = fn
	s.mu.Unlock()
}

// SetTimeout configures how long Approve blocks before auto-denying.
func (s *ServerApprover) SetTimeout(d time.Duration) {
	s.mu.Lock()
	s.timeout = d
	s.mu.Unlock()
}

// Approve implements the Approver interface. It registers the request,
// notifies the UI, and blocks for the user's decision (which may carry
// free-form feedback the user typed into the popup).
func (s *ServerApprover) Approve(req ApprovalRequest) Verdict {
	id := fmt.Sprintf("appr-%d", atomic.AddUint64(&s.counter, 1))
	entry := &pendingEntry{
		request: req,
		created: time.Now(),
		ch:      make(chan Verdict, 1),
	}

	s.mu.Lock()
	s.pending[id] = entry
	onReq := s.onRequest
	timeout := s.timeout
	s.mu.Unlock()

	if onReq != nil {
		// Notify the UI (non-blocking — the SSE broadcast is async anyway).
		onReq(id, req)
	}

	select {
	case v := <-entry.ch:
		return v
	case <-time.After(timeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return Verdict{Decision: DecisionDeny, Feedback: "approval timed out (no response from UI)"}
	}
}

// Resolve delivers the user's decision (and optional feedback) to the blocked
// Approve call. Returns false if the id is unknown (already resolved or
// expired).
func (s *ServerApprover) Resolve(id string, decision Decision, feedback string) bool {
	s.mu.Lock()
	entry, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	entry.ch <- Verdict{Decision: decision, Feedback: feedback}
	return true
}

// Pending returns all unresolved approval requests (for GET /api/approvals).
func (s *ServerApprover) Pending() []PendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PendingApproval, 0, len(s.pending))
	for id, e := range s.pending {
		out = append(out, PendingApproval{
			ID:      id,
			Tool:    e.request.Tool,
			Summary: e.request.Summary,
			Preview: e.request.Preview,
			Risk:    string(e.request.Risk),
			Created: e.created,
		})
	}
	return out
}

// ParseDecision converts a string from the web UI ("allow-once",
// "allow-session", "deny") into a Decision. Unknown strings default to Deny.
func ParseDecision(s string) Decision {
	switch s {
	case "allow-once", "once", "1":
		return DecisionAllowOnce
	case "allow-session", "session", "2":
		return DecisionAllowSession
	default:
		return DecisionDeny
	}
}

// CancelAll resolves all pending requests with a Deny verdict and clears the pending map.
func (s *ServerApprover) CancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.pending {
		entry.ch <- Verdict{Decision: DecisionDeny, Feedback: "execution cancelled"}
		delete(s.pending, id)
	}
}
