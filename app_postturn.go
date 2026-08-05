package main

// app_postturn.go — the work that follows a turn, wired once for every surface.
//
// The console used to do this inline in a goroutine and the web did its own
// version in the chat handler, while the editor and headless paths did nothing.
// Registering it on the uiport manager means a surface gets it by existing
// rather than by remembering to.

import (
	"context"

	"github.com/darkcode/planwork"
	"github.com/darkcode/uiport"
)

// projectRefresh adapts planwork.Refresher to uiport.PostTurn.
type projectRefresh struct{ r *planwork.Refresher }

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
	p.r.Refresh(ctx, req.Project, req.Query, output)
}

// newPostTurnHooks builds the post-turn work shared by every surface. Returns
// nothing when the pieces are missing, so a build without a project store
// simply has no post-turn step.
func (a *AppRunner) newPostTurnHooks() []uiport.Option {
	var opts []uiport.Option
	if r := planwork.NewRefresher(a.ProjectStore, a.Kernel, a.Emitter); r != nil {
		opts = append(opts, uiport.WithPostTurn(projectRefresh{r: r}))
	}
	return opts
}
