package main

// app_postturn.go — the work that follows a turn, wired once for every surface.
//
// The console used to do this inline in a goroutine and the web did its own
// version in the chat handler, while the editor and headless paths did nothing.
// Registering it on the uiport manager means a surface gets it by existing
// rather than by remembering to.

import (
	"context"

	"github.com/darkcode/hooks"
	"github.com/darkcode/planwork"
	"github.com/darkcode/uiport"
)

// sequentialReporter reports whether the agent is running one call at a time.
// Satisfied by the kernel.
type sequentialReporter interface{ SequentialMode() bool }

// projectRefresh adapts planwork.Refresher to uiport.PostTurn.
type projectRefresh struct {
	r   *planwork.Refresher
	seq sequentialReporter
}

// AfterTurn rewrites the active project's plan and workflow. Nothing happens
// when the request named no project, which is the common case.
func (p projectRefresh) AfterTurn(ctx context.Context, req uiport.Request, output string) {
	if p.r == nil || req.Project == "" || req.PlanAlreadyAmended {
		return
	}
	// Chat and General turns answer without building anything, so there is no
	// new state for a plan to reflect and the call would be pure cost.
	if req.Tools == "off" || req.Tools == "readonly" {
		return
	}
	// Sequential mode is the default for free-tier cloud models, where the
	// budget is a small number of requests PER DAY. An extra call here runs
	// alongside the user's next request and competes for that budget, and the
	// 429 it earns looks like the agent refusing to work rather than like a
	// plan refresh. The plan catches up on the next parallel turn.
	if p.seq != nil && p.seq.SequentialMode() {
		return
	}
	p.r.Refresh(ctx, req.Project, req.Query, output)
}

// turnEndHook fires the user's turn_end hooks. It rides the same post-turn
// registration as the plan refresh, for the same reason: a surface gets it by
// existing rather than by remembering to.
type turnEndHook struct{ h *hooks.Manager }

func (t turnEndHook) AfterTurn(ctx context.Context, req uiport.Request, output string) {
	// turn_end cannot fail a turn that has already produced its answer, so the
	// error is discarded rather than ignored by accident — see package hooks.
	_ = t.h.Run(ctx, hooks.TurnEnd, hooks.Context{
		Goal:    req.Query,
		Success: output != "",
	})
}

// newPostTurnHooks builds the post-turn work shared by every surface. Returns
// nothing when the pieces are missing, so a build without a project store
// simply has no post-turn step.
func (a *AppRunner) newPostTurnHooks() []uiport.Option {
	var opts []uiport.Option
	if r := planwork.NewRefresher(a.ProjectStore, a.Kernel, a.Emitter); r != nil {
		opts = append(opts, uiport.WithPostTurn(projectRefresh{r: r, seq: a.Kernel}))
	}
	if a.Hooks.Configured(hooks.TurnEnd) {
		opts = append(opts, uiport.WithPostTurn(turnEndHook{a.Hooks}))
	}
	return opts
}
