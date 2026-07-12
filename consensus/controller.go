package consensus

import (
	"context"

	"github.com/darkcode/core"
	"github.com/darkcode/ui"
)

// Controller manages multi-model consensus across all execution modes.
type Controller struct {
	router  core.ModelRouter
	emitter *ui.EventEmitter
}

func NewController(router core.ModelRouter, emitter *ui.EventEmitter) *Controller {
	return &Controller{
		router:  router,
		emitter: emitter,
	}
}

// TextConsensus runs consensus on pure text (General mode, no tools).
func (c *Controller) TextConsensus(ctx context.Context, messages []core.Message, goal string) (string, error) {
	// Full implementation would extract from router.Consensus()
	res, err := c.router.Consensus(ctx, messages, goal)
	if err != nil {
		return "", err
	}
	return res.Synthesized, nil
}

// PostExecutionConsensus refines tool-grounded output (Loop mode).
// The toolTrace is injected so reviewers cannot hallucinate.
func (c *Controller) PostExecutionConsensus(ctx context.Context, goal, output, toolTrace string) (string, error) {
	// For now, delegate back to the standard consensus flow
	res, err := c.router.Consensus(ctx, []core.Message{
		{Role: core.RoleUser, Content: goal + "\nOutput:\n" + output + "\nTools:\n" + toolTrace},
	}, goal)
	if err != nil {
		return "", err
	}
	return res.Synthesized, nil
}
