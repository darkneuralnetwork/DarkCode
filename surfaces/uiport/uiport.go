// Package uiport is the single entry point from a user surface into the agent.
//
// # WHY THIS EXISTS
//
// There were six places that called Kernel.Execute — the interactive console,
// the headless -q path, the ACP executor, two in the web chat handler, and the
// OpenAI-compatible endpoint. Each independently decided what to put on the
// request: whether to set the workspace, whether to set the project, whether to
// install an approver, whether to apply the verb overrides.
//
// They did not agree, and the disagreement was not cosmetic. Both CLI surfaces
// never set core.WorkspaceKey at all, and confineWrite returns nil when the
// workspace is empty — so path confinement, the control that stops the agent
// writing outside your repository, was inert on the entire CLI. The guard
// itself works: the same write with a workspace set is refused. Nobody had
// armed it. That is what a sixth setup site costs.
//
// So the request type here cannot be built incompletely. Execute refuses a
// Request with no workspace instead of running one without confinement, which
// makes "a surface forgot" a startup error rather than a silent permission.
//
// # WHAT BELONGS HERE
//
// Presentation and request setup. This package holds no business logic: it
// does not decide strategy, does not talk to a model, does not touch memory.
// It turns "a user asked for something on some surface" into a fully-formed
// request, runs it, and reports what happened.
package uiport

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/darkcode/infra/core"
)

// Surface names where a request came from. It is recorded on the request so
// telemetry can attribute cost and latency per surface, and so a future policy
// can differ by surface without another entry point being added to do it.
type Surface string

// A surface exists here only if a human is on the other end of it. The
// headless `-q` path was removed: it was a fourth implementation of "run one
// turn", it auto-approved every permission prompt, and scripting is better
// served by the OpenAI-compatible /v1 endpoint, which any client already
// speaks and which sits behind the same CSRF, content-type and rate-limit
// middleware as the rest of the server.
const (
	SurfaceCLI Surface = "cli" // interactive console
	SurfaceGUI Surface = "gui" // web chat
	SurfaceACP Surface = "acp" // editor, over Agent Client Protocol
	SurfaceAPI Surface = "api" // OpenAI-compatible endpoint
)

// Engine is the part of the orchestration kernel a surface may reach. It is an
// interface so this package does not depend on the concrete kernel, and so the
// dependency runs one way: surfaces → uiport → orchestrator, never back.
type Engine interface {
	Execute(ctx context.Context, goal string) (string, error)
	ApplyRequestOverrides(ctx context.Context, mode, safety, loop, tools, brain string) (context.Context, func())
	WithPlanOverride(ctx context.Context, mode string) context.Context
}

// Request is everything needed to run one turn. Workspace is mandatory; see
// the package comment for what happens when a surface omits it.
type Request struct {
	// Query is the user's message.
	Query string

	// Surface is where it came from.
	Surface Surface

	// Workspace is the directory the agent is confined to. Required.
	Workspace string

	// Project optionally names the active project, injected into context so
	// the kernel's planner follows its plan.
	Project string

	// Mode, Safety and Brain override routing mode, permission level and
	// local/cloud selection for this request. Empty leaves each configured
	// value alone. These still mutate shared router/gate state under a depth
	// counter — see orchestrator/request_state.go.
	Mode, Safety, Brain string

	// Loop, Tools and Plan are the verb decisions: "on"/"off",
	// "off"/"readonly"/"on", and "always"/"never". Empty means no override.
	// These ride on the request's context and cannot be seen by another
	// request.
	Loop, Tools, Plan string

	// PlanAlreadyAmended tells the post-turn refresh to stand down because the
	// caller rewrote the project plan BEFORE running the turn.
	//
	// The web does that deliberately — amending first is what lets the plan
	// drive the execution rather than merely describe it afterwards — and
	// without this flag the shared post-turn hook would rewrite the same two
	// documents a second time, spending two model calls per turn where the
	// point of sharing the hook was to spend one.
	PlanAlreadyAmended bool
}

// Validate reports why a request cannot be run, or nil.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Query) == "" {
		return fmt.Errorf("uiport: empty query")
	}
	if r.Surface == "" {
		return fmt.Errorf("uiport: request has no surface")
	}
	if strings.TrimSpace(r.Workspace) == "" {
		// Deliberately fatal rather than defaulted. Defaulting to the process
		// working directory here is what the CLI effectively did by omission,
		// except without confinement: an unset workspace means confineWrite
		// permits every path. A surface that does not know its workspace must
		// say so.
		return fmt.Errorf("uiport: request from %q has no workspace — "+
			"path confinement cannot be enforced without one", r.Surface)
	}
	return nil
}

// PostTurn is work that runs after a turn succeeds, on every surface.
//
// It exists because the surfaces had drifted into doing different amounts of
// it: the web ran seven post-turn steps, the console ran one, and the editor
// and headless paths ran none. The same request produced a different result
// depending on which door it came through — a project's plan updated when you
// asked from the browser and silently did not when you asked from the terminal.
//
// An implementation must not fail the turn. The work already succeeded and the
// user already has an answer; a plan refresh that errors is a stale plan, not a
// failed request.
type PostTurn interface {
	AfterTurn(ctx context.Context, req Request, output string)
}

// Manager is the presentation layer's handle on the agent.
type Manager struct {
	engine   Engine
	postTurn []PostTurn
}

// Option configures a Manager.
type Option func(*Manager)

// WithPostTurn registers work to run after every successful turn on every
// surface. Registering it here rather than in a handler is the point: a new
// surface gets it without knowing it exists.
func WithPostTurn(p PostTurn) Option {
	return func(m *Manager) {
		if p != nil {
			m.postTurn = append(m.postTurn, p)
		}
	}
}

// New returns a Manager over engine. A nil engine is refused here rather than
// panicking later inside a goroutine, where no recover can see it.
func New(engine Engine, opts ...Option) (*Manager, error) {
	if engine == nil {
		return nil, fmt.Errorf("uiport: nil engine")
	}
	m := &Manager{engine: engine}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Execute runs one turn and returns its output.
//
// The context it builds is the same for every surface: workspace, project,
// then the verb overrides. That sameness is the point — it is why a new
// surface cannot arrive with its own idea of what a request needs.
func (m *Manager) Execute(ctx context.Context, req Request) (string, error) {
	if err := req.Validate(); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Absolute, so confineWrite compares like with like regardless of how the
	// surface phrased it.
	ws := req.Workspace
	if abs, err := filepath.Abs(ws); err == nil {
		ws = abs
	}
	ctx = context.WithValue(ctx, core.WorkspaceKey, ws)
	ctx = context.WithValue(ctx, core.ProjectKey, req.Project)

	ctx, restore := m.engine.ApplyRequestOverrides(ctx, req.Mode, req.Safety, req.Loop, req.Tools, req.Brain)
	defer restore()
	ctx = m.engine.WithPlanOverride(ctx, req.Plan)

	out, err := m.engine.Execute(ctx, req.Query)
	if err != nil {
		return "", err
	}

	// Post-turn work runs here so it runs identically for every surface. A
	// panic in it must not take down a turn that has already succeeded, nor
	// the process: this can be called from an HTTP handler goroutine where
	// neither net/http's per-connection recovery nor the recover middleware
	// can see it.
	for _, p := range m.postTurn {
		func() {
			defer func() { _ = recover() }()
			p.AfterTurn(ctx, req, out)
		}()
	}
	return out, nil
}
