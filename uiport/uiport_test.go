package uiport

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// fakeEngine records the context Execute was handed, so the tests can assert
// what a surface's request actually carries rather than what it meant to.
type fakeEngine struct {
	gotCtx   context.Context
	gotGoal  string
	overrode [5]string
	planMode string
	out      string
	err      error
}

func (f *fakeEngine) Execute(ctx context.Context, goal string) (string, error) {
	f.gotCtx, f.gotGoal = ctx, goal
	return f.out, f.err
}

func (f *fakeEngine) ApplyRequestOverrides(ctx context.Context, mode, safety, loop, tools, brain string) (context.Context, func()) {
	f.overrode = [5]string{mode, safety, loop, tools, brain}
	return ctx, func() {}
}

func (f *fakeEngine) WithPlanOverride(ctx context.Context, mode string) context.Context {
	f.planMode = mode
	return ctx
}

func newManager(t *testing.T) (*Manager, *fakeEngine) {
	t.Helper()
	fe := &fakeEngine{out: "done"}
	m, err := New(fe)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, fe
}

// TestEveryRequestCarriesAWorkspace is the regression for the defect this
// package was built to close.
//
// Both CLI surfaces called Kernel.Execute with a context that never had
// core.WorkspaceKey set, and confineWrite returns nil when the workspace is
// empty — so path confinement was inert on the whole CLI. Measured before the
// change: a write to an arbitrary path outside any workspace succeeded from a
// CLI-shaped context, while the identical write with a workspace set was
// refused. The guard worked; nobody armed it.
func TestEveryRequestCarriesAWorkspace(t *testing.T) {
	m, fe := newManager(t)

	if _, err := m.Execute(context.Background(), Request{
		Query: "do a thing", Surface: SurfaceACP, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ws, _ := fe.gotCtx.Value(core.WorkspaceKey).(string)
	if ws == "" {
		t.Fatal("the request reached the kernel with no workspace — " +
			"confineWrite permits every path in that state")
	}
}

// TestRequestWithoutWorkspaceIsRefused pins the fail-closed direction. A
// surface that does not set a workspace must get an error, not a run without
// confinement.
func TestRequestWithoutWorkspaceIsRefused(t *testing.T) {
	m, fe := newManager(t)

	_, err := m.Execute(context.Background(), Request{
		Query: "write /etc/passwd", Surface: SurfaceCLI,
	})
	if err == nil {
		t.Fatal("a request with no workspace ran anyway — this is the CLI defect restored")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("error = %q, want it to name the missing workspace", err)
	}
	if fe.gotGoal != "" {
		t.Error("the kernel was reached despite the request being invalid")
	}
}

func TestEmptyQueryAndMissingSurfaceAreRefused(t *testing.T) {
	m, _ := newManager(t)
	ws := t.TempDir()

	if _, err := m.Execute(context.Background(), Request{
		Query: "   ", Surface: SurfaceCLI, Workspace: ws,
	}); err == nil {
		t.Error("an empty query was accepted")
	}
	if _, err := m.Execute(context.Background(), Request{
		Query: "hi", Workspace: ws,
	}); err == nil {
		t.Error("a request with no surface was accepted")
	}
}

// TestWorkspaceIsAbsolute — confineWrite compares the target against the
// workspace root, so a relative root would compare against the process working
// directory instead of the one the surface named.
func TestWorkspaceIsAbsolute(t *testing.T) {
	m, fe := newManager(t)

	if _, err := m.Execute(context.Background(), Request{
		Query: "x", Surface: SurfaceCLI, Workspace: ".",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ws, _ := fe.gotCtx.Value(core.WorkspaceKey).(string)
	if !strings.HasPrefix(ws, "/") {
		t.Errorf("workspace on the context = %q, want an absolute path", ws)
	}
}

// TestOverridesAndProjectReachTheEngine — every surface's settings must arrive
// through the one path, so no surface needs its own.
func TestOverridesAndProjectReachTheEngine(t *testing.T) {
	m, fe := newManager(t)

	if _, err := m.Execute(context.Background(), Request{
		Query: "x", Surface: SurfaceGUI, Workspace: t.TempDir(),
		Project: "proj-1",
		Mode:    "consensus", Safety: "strict", Brain: "local",
		Loop: "on", Tools: "readonly", Plan: "always",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := fe.overrode; got != [5]string{"consensus", "strict", "on", "readonly", "local"} {
		t.Errorf("overrides reaching the kernel = %v", got)
	}
	if fe.planMode != "always" {
		t.Errorf("plan override = %q, want always", fe.planMode)
	}
	if p, _ := fe.gotCtx.Value(core.ProjectKey).(string); p != "proj-1" {
		t.Errorf("project on the context = %q, want proj-1", p)
	}
}

func TestNilEngineIsRefused(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("New(nil) returned no error — a nil engine panics inside Execute " +
			"where no recover can see it")
	}
}

// ── post-turn parity ────────────────────────────────────────────────────────

type recordingPostTurn struct {
	calls []Request
	panic bool
}

func (r *recordingPostTurn) AfterTurn(ctx context.Context, req Request, out string) {
	r.calls = append(r.calls, req)
	if r.panic {
		panic("post-turn work exploded")
	}
}

// TestPostTurnRunsForEverySurface is the parity regression. The web ran seven
// post-turn steps, the console one, and the editor and headless paths none, so
// a project's plan updated when you asked from the browser and silently did not
// when you asked from the terminal.
func TestPostTurnRunsForEverySurface(t *testing.T) {
	for _, surface := range []Surface{SurfaceCLI, SurfaceGUI, SurfaceACP, SurfaceAPI} {
		t.Run(string(surface), func(t *testing.T) {
			rec := &recordingPostTurn{}
			m, err := New(&fakeEngine{out: "done"}, WithPostTurn(rec))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Execute(context.Background(), Request{
				Query: "x", Surface: surface, Workspace: t.TempDir(), Project: "p1",
			}); err != nil {
				t.Fatal(err)
			}
			if len(rec.calls) != 1 {
				t.Errorf("%s ran post-turn work %d times, want 1 — surfaces have drifted apart again", surface, len(rec.calls))
			}
		})
	}
}

// TestPostTurnDoesNotRunWhenTheTurnFailed — there is no new state to reflect.
func TestPostTurnDoesNotRunWhenTheTurnFailed(t *testing.T) {
	rec := &recordingPostTurn{}
	m, _ := New(&fakeEngine{err: context.DeadlineExceeded}, WithPostTurn(rec))

	if _, err := m.Execute(context.Background(), Request{
		Query: "x", Surface: SurfaceCLI, Workspace: t.TempDir(), Project: "p1",
	}); err == nil {
		t.Fatal("expected the engine error to surface")
	}
	if len(rec.calls) != 0 {
		t.Error("post-turn work ran after a failed turn")
	}
}

// TestPostTurnPanicDoesNotLoseTheAnswer — this runs in an HTTP handler
// goroutine where neither net/http's per-connection recovery nor the recover
// middleware can see it, so a panic here would take down the process and the
// user would lose a turn that had already succeeded.
func TestPostTurnPanicDoesNotLoseTheAnswer(t *testing.T) {
	rec := &recordingPostTurn{panic: true}
	m, _ := New(&fakeEngine{out: "the answer"}, WithPostTurn(rec))

	out, err := m.Execute(context.Background(), Request{
		Query: "x", Surface: SurfaceGUI, Workspace: t.TempDir(), Project: "p1",
	})
	if err != nil {
		t.Fatalf("a panic in post-turn work failed the turn: %v", err)
	}
	if out != "the answer" {
		t.Errorf("output = %q, want the engine's answer", out)
	}
}

func TestPlanAlreadyAmendedReachesTheHook(t *testing.T) {
	rec := &recordingPostTurn{}
	m, _ := New(&fakeEngine{out: "done"}, WithPostTurn(rec))

	if _, err := m.Execute(context.Background(), Request{
		Query: "x", Surface: SurfaceGUI, Workspace: t.TempDir(),
		Project: "p1", PlanAlreadyAmended: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || !rec.calls[0].PlanAlreadyAmended {
		t.Error("the hook cannot tell the plan was already amended, so it will spend a second call")
	}
}
