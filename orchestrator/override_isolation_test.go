package orchestrator

import (
	"context"
	"testing"
)

// override_isolation_test.go — a request must execute under its OWN overrides.
//
// override_scope_test.go already pins the other half of this: after two
// overlapping requests, the router and gate return to their configured base.
// That is about what LEAKS AFTERWARDS, and it is fixed.
//
// Nothing pinned what a request sees WHILE it runs. The per-request flags were
// single fields on the shared Kernel — one *bool for the whole process — so the
// second request to arrive overwrote the first request's answer and the first
// request kept executing under settings it never asked for. The depth counters
// added for the leak cannot help here: they make the restore correct, not the
// live value.
//
// This is reachable on every concurrent surface. Two browser tabs, an /api/chat
// turn overlapping a /v1 call, or the CLI's background plan refresh landing
// while a /loop query is still iterating.

// TestOverlappingRequestsKeepTheirOwnLoopVerb is the regression. A asks to
// iterate, B explicitly asks not to, and A must still be iterating.
func TestOverlappingRequestsKeepTheirOwnLoopVerb(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	ctxA, restoreA := k.ApplyRequestOverrides(context.Background(), "", "", "on", "", "")
	defer restoreA()

	if !k.loopEnabledForRequest(ctxA) {
		t.Fatal("A asked for /loop and is not looping even before B exists")
	}

	// B arrives while A is still running and asks for the opposite.
	ctxB, restoreB := k.ApplyRequestOverrides(context.Background(), "", "", "off", "", "")
	defer restoreB()

	if !k.loopEnabledForRequest(ctxA) {
		t.Error("A asked for /loop, B asked for no loop, and A stopped looping — " +
			"B's verb reached into a request that was already running")
	}
	if k.loopEnabledForRequest(ctxB) {
		t.Error("B asked for no loop and is looping — A's verb reached forward into B")
	}
}

// TestOverlappingRequestsKeepTheirOwnToolScope is the same collision on tool
// access, where the consequence is worse than a wasted call: a Chat request
// pinned read-only can be handed the mutating toolset by a Build request that
// merely started later.
func TestOverlappingRequestsKeepTheirOwnToolScope(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	// A is a Chat turn: tools available, but read-only ones only.
	ctxA, restoreA := k.ApplyRequestOverrides(context.Background(), "", "", "", "readonly", "")
	defer restoreA()

	// B is a Build turn: full mutating toolset.
	ctxB, restoreB := k.ApplyRequestOverrides(context.Background(), "", "", "", "on", "")
	defer restoreB()

	if !k.readOnlyForRequest(ctxA) {
		t.Error("A was pinned read-only and is no longer — a Chat turn can now write " +
			"files because an unrelated Build turn started after it")
	}
	if k.readOnlyForRequest(ctxB) {
		t.Error("B asked for the full toolset and was forced read-only by A")
	}
}

// TestToolsOffMeansReadOnlyNotToolless pins the replacement for General mode.
//
// "off" used to disable tools entirely. A turn in that mode could not search
// the web, read a PDF, or look at a file — asked what was in a directory, the
// agent answered that it could not see the files, because it had been given no
// way to look. A mode that cannot check anything does not answer more cheaply,
// it answers more confidently and less correctly.
//
// "off" now means the same as read-only: tools are offered, none of them can
// change anything. The wire value is kept so an older client still works.
func TestToolsOffMeansReadOnlyNotToolless(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	ctxA, restoreA := k.ApplyRequestOverrides(context.Background(), "", "", "", "off", "")
	defer restoreA()

	if k.toolsDisabledForRequest(ctxA) {
		t.Error("tools=off still disables tools outright — the turn cannot look anything up")
	}
	if !k.readOnlyForRequest(ctxA) {
		t.Error("tools=off is not read-only, so a conversational turn could write files")
	}

	// And it is still isolated from a concurrent Build turn.
	ctxB, restoreB := k.ApplyRequestOverrides(context.Background(), "", "", "", "on", "")
	defer restoreB()
	if !k.readOnlyForRequest(ctxA) {
		t.Error("A was pinned read-only and lost it when B started")
	}
	if k.readOnlyForRequest(ctxB) {
		t.Error("B asked for the full toolset and was forced read-only by A")
	}
}

// TestRequestWithNoOverridesIsUnaffectedByOthers pins the default path: a plain
// request carries no overrides and must not inherit a concurrent one's.
func TestRequestWithNoOverridesIsUnaffectedByOthers(t *testing.T) {
	deps := newTestKernel(t, nil)
	k := deps.Kernel

	plain := context.Background()

	_, restoreOther := k.ApplyRequestOverrides(context.Background(), "", "", "on", "off", "")
	defer restoreOther()

	if k.loopEnabledForRequest(plain) {
		t.Error("a request with no verb started looping because another request used /loop")
	}
	if k.readOnlyForRequest(plain) {
		t.Error("a request with no verb was pinned read-only by a concurrent chat turn")
	}
}
