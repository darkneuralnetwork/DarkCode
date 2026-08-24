package orchestrator

// request_state.go — the settings that belong to ONE request.
//
// These used to be fields on the shared Kernel: one *bool per process for
// "is this request looping", "does this request get tools", "is this request
// read-only". A single field cannot answer a question that has a different
// answer per request, so the second request to arrive overwrote the first
// one's answer and the first kept running under settings it never asked for.
//
// override_scope_test.go had already pinned the half of this that leaks
// afterwards, and the depth counters fixed it: after every request finishes,
// the router and gate return to their configured base. But a depth counter
// makes the RESTORE correct, not the LIVE value — while both requests are in
// flight there is still only one field, and it holds whichever value was
// written last.
//
// Measured before changing anything: request A asks for /loop, request B asks
// for no loop, and A stops looping. Worse on tool scope — a Chat turn pinned
// read-only starts answering with the mutating toolset because an unrelated
// Build turn started after it. See override_isolation_test.go.
//
// The fix is to put the state where the request already is. context.Context is
// per-request by construction and is already threaded through every read site,
// so there is no field for a second request to overwrite. Nothing is shared,
// so nothing needs a lock, a depth counter, or a restore.
//
// Router mode, gate safety level and the brain selector are NOT here. Those
// live on shared router/gate objects that a request does not own, so they still
// use the save/depth/restore mechanism in ApplyRequestOverrides. Moving them
// needs those APIs to take a request scope, which is a larger change than this
// one; until then they remain last-writer-wins while requests overlap.

import "context"

// requestState is the immutable set of per-request overrides. A nil pointer
// means "not overridden" and the reader falls back to the default, exactly as
// the kernel fields did.
type requestState struct {
	loop          *bool // /loop verb or Loop chat mode
	toolsDisabled *bool // General mode: no tools offered at all
	readOnly      *bool // Chat mode: read-only tools only
	plan          *bool // /graph forces planning, /loop leaves it adaptive
}

// requestStateKey is unexported so no other package can put a value under it —
// the state can only be set through ApplyRequestOverrides.
type requestStateKey struct{}

// withRequestState returns a context carrying rs. It never returns a nil ctx.
func withRequestState(ctx context.Context, rs *requestState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if rs == nil {
		return ctx
	}
	return context.WithValue(ctx, requestStateKey{}, rs)
}

// requestStateFrom returns the state carried by ctx, or nil when the request
// carries no overrides. A nil ctx is treated as no overrides rather than
// panicking: several kernel paths build sub-contexts, and a missing override
// must degrade to the default, never to "permitted".
func requestStateFrom(ctx context.Context) *requestState {
	if ctx == nil {
		return nil
	}
	rs, _ := ctx.Value(requestStateKey{}).(*requestState)
	return rs
}

// loopEnabledForRequest reports whether the ReAct loop should run for this
// request. Iteration is a per-message decision — the /loop verb or the Loop
// chat mode — so with no override the answer is no.
func (k *Kernel) loopEnabledForRequest(ctx context.Context) bool {
	if rs := requestStateFrom(ctx); rs != nil && rs.loop != nil {
		return *rs.loop
	}
	return false
}

// toolsDisabledForRequest reports whether tool access is disabled for this
// request (General mode fast path). When true, Execute takes a lightweight
// single-call path with NO tools offered to the LLM — no DAG, no worker
// agents, no approval popups.
func (k *Kernel) toolsDisabledForRequest(ctx context.Context) bool {
	if rs := requestStateFrom(ctx); rs != nil && rs.toolsDisabled != nil {
		return *rs.toolsDisabled
	}
	return false
}

// readOnlyForRequest reports whether this is a Chat (read-only) request: only
// read-only tools are offered and mutating tools are refused.
//
// The default is false — the full toolset — which matches the previous
// behaviour, but note that it is the permissive direction. It is safe only
// because this answers "did the caller ask to be restricted", not "is this
// caller allowed to write": the permission gate and path confinement decide
// that, and they fail closed independently of this flag.
func (k *Kernel) readOnlyForRequest(ctx context.Context) bool {
	rs := requestStateFrom(ctx)
	return rs != nil && rs.readOnly != nil && *rs.readOnly
}

// planForced reports the per-request planning override, if any. This is what
// separates /graph from /loop: both iterate, but /graph always decomposes into
// a task graph first so there are per-task acceptance criteria to prove.
//
// It shared the same defect as the flags above — an overlapping /loop turn
// could clear a /graph turn's forced planning, silently downgrading /graph to
// its synonym.
func (k *Kernel) planForced(ctx context.Context) (force bool, set bool) {
	rs := requestStateFrom(ctx)
	if rs == nil || rs.plan == nil {
		return false, false
	}
	return *rs.plan, true
}

// WithPlanOverride returns a context forcing the planning phase on ("always")
// or off ("never") for one request. Any other value leaves the adaptive
// decision alone and returns ctx unchanged.
//
// This replaces ApplyPlanOverride, which mutated a shared kernel field and
// returned a restore func. There is nothing to restore now, so there is no way
// to forget to.
func (k *Kernel) WithPlanOverride(ctx context.Context, mode string) context.Context {
	if mode != "always" && mode != "never" {
		return ctx
	}
	v := mode == "always"
	// Preserve any flags already on the context — the surfaces apply the verb
	// overrides first and the plan override second, on the same request.
	rs := &requestState{plan: &v}
	if prev := requestStateFrom(ctx); prev != nil {
		rs.loop, rs.toolsDisabled, rs.readOnly = prev.loop, prev.toolsDisabled, prev.readOnly
	}
	return withRequestState(ctx, rs)
}
