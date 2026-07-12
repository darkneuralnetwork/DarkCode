package permission

// ============================================================================
// MODE-AWARE APPROVER
//
// The single source of truth for HOW approval prompts are delivered to the
// user. It is installed ONCE on the permission gate and delegates each prompt
// to the right sub-approver based on the currently active UI mode:
//
//   • GUI mode → ServerApprover (blocks the tool goroutine, broadcasts an
//                SSE "approval/request" event, the browser shows a popup,
//                the user's choice comes back via /api/approvals/decide).
//   • CLI mode → a terminal Approver (Console.requestApproval) that reads
//                from stdin with a colored prompt.
//
// WHY THIS EXISTS
//
// Previously the gate held a single Approver function pointer. main.go set it
// to ServerApprover.Approve at startup; the CLI console overwrote it with the
// terminal approver on entry. The bug: switching CLI → GUI never restored the
// ServerApprover, so in GUI mode (after a CLI session) destructive tool calls
// invoked the terminal approver, which blocked on a readline that nobody was
// reading — the GUI popup never appeared, the request hung for 5 minutes, and
// then auto-denied. "Permission taken in the wrong mode."
//
// The ModeAwareApprover fixes this at the root: there is ONE approver on the
// gate, and it consults an atomic mode flag to decide where the prompt goes.
// Switching modes just flips the flag (SetMode) — no approver re-installation,
// no stale function pointers, no race between the two modes.
//
// It is safe for concurrent use: the gate serializes prompts (promptMu), and
// the mode flag is read atomically.
// ============================================================================

import "sync/atomic"

// UIMode identifies which surface is currently driving interaction.
type UIMode int32

const (
	// ModeCLI means the terminal console is reading input; approval prompts
	// must be rendered inline and read from stdin.
	ModeCLI UIMode = iota
	// ModeGUI means a browser is connected via SSE; approval prompts must be
	// delivered as SSE events and resolved through the HTTP approval API.
	ModeGUI
)

// ModeAwareApprover is a composite Approver that routes each prompt to the
// sub-approver matching the active UI mode. Install it once on the gate; flip
// the mode with SetMode whenever the process switches surfaces.
type ModeAwareApprover struct {
	// mode is read/written atomically (UIMode is int32-backed).
	mode uint32 // 0 = CLI, 1 = GUI

	// gui is the server-backed approver used in GUI mode. It is responsible
	// for broadcasting the SSE request event and blocking until the browser
	// resolves it (or the timeout elapses).
	gui *ServerApprover

	// cli is the terminal approver used in CLI mode (set by the console when
	// it starts). nil until the console installs one.
	cli atomic.Value // Approver
}

// NewModeAwareApprover wraps a ServerApprover (for GUI mode). The CLI delegate
// is installed later by the console via SetCLIApprover. The initial mode is
// CLI — main.go flips it to GUI when the browser is launched.
func NewModeAwareApprover(gui *ServerApprover) *ModeAwareApprover {
	m := &ModeAwareApprover{gui: gui}
	m.setMode(ModeCLI)
	return m
}

// setMode stores the active UI mode atomically.
func (m *ModeAwareApprover) setMode(mode UIMode) {
	atomic.StoreUint32(&m.mode, uint32(mode))
}

// SetMode flips the active UI mode (GUI or CLI). Safe to call at any time;
// the very next approval prompt will be routed to the new mode's approver.
func (m *ModeAwareApprover) SetMode(mode UIMode) { m.setMode(mode) }

// Mode returns the currently active UI mode.
func (m *ModeAwareApprover) Mode() UIMode { return UIMode(atomic.LoadUint32(&m.mode)) }

// SetCLIApprover installs the terminal approver used in CLI mode. Called by
// the console when it starts (and again if the delegate changes).
func (m *ModeAwareApprover) SetCLIApprover(a Approver) {
	if a == nil {
		a = AutoDeny()
	}
	m.cli.Store(a)
}

// Approve implements the Approver interface. It routes the prompt to the
// active mode's sub-approver. In GUI mode it always uses the ServerApprover
// (which blocks for the browser). In CLI mode it uses the terminal delegate;
// if none is installed yet it fails safe (deny) rather than hanging.
func (m *ModeAwareApprover) Approve(req ApprovalRequest) Verdict {
	if m.Mode() == ModeGUI && m.gui != nil {
		return m.gui.Approve(req)
	}
	if v := m.cli.Load(); v != nil {
		return v.(Approver)(req)
	}
	// No terminal delegate and not in GUI mode: fail safe (deny) with an
	// explanation rather than blocking forever on an unanswerable prompt.
	return Verdict{Decision: DecisionDeny, Feedback: "no approval surface available in the active mode"}
}

// GUIApprover returns the wrapped ServerApprover (so the server can wire its
// OnRequest/Resolve/Pending methods). Returns nil if none was provided.
func (m *ModeAwareApprover) GUIApprover() *ServerApprover { return m.gui }
