// Package modelport is the single place that decides which model runs a call
// and under what limits.
//
// # WHY THIS EXISTS
//
// Twenty-one sites called a model directly, and each invented its own policy.
// Temperature was chosen per site — 0.2 here, 0.5 there, 0.7 somewhere else —
// with no way to see the set or to know whether the spread was deliberate.
//
// More expensively, eight of the eighteen completion sites sent no MaxTokens
// at all, so the ceiling was whatever the provider defaults to — usually the
// rest of the context window. An unbounded reply is an unbounded bill, and the
// eight were not the obscure ones: the ReAct loop's main call, every sub-agent
// worker turn, and every conversational answer.
//
// (An earlier draft of this comment said two of twenty-one, from a grep that
// missed most of the MaxTokens assignments. The number was recounted by
// parsing each call's surrounding request instead.)
//
// So a caller states its PURPOSE and the manager supplies the tier and the
// limits. Ask has no Model and no Tier field on purpose: a caller that can
// name a model is a caller that will name the wrong one, and then the routing
// policy lives in twenty-one places again.
//
// # WHAT THIS DOES NOT DO
//
// It does not know about memory, retrieval, or orchestration. It takes
// messages and returns text. Every attempt to let a model manager reach into
// memory ends with the model layer importing the store, which is the coupling
// the architecture exists to prevent.
package modelport

import (
	"context"
	"fmt"

	"github.com/darkcode/core"
)

// Purpose is what a call is FOR. It decides the tier and the limits.
type Purpose string

const (
	// PurposePlan decomposes work. Reasoning tier, low temperature, generous
	// ceiling — a truncated plan is worse than no plan.
	PurposePlan Purpose = "plan"

	// PurposeExecute does the work. Coding tier.
	PurposeExecute Purpose = "execute"

	// PurposeConverse answers a person. Warmer, because the alternative reads
	// like a manual.
	PurposeConverse Purpose = "converse"

	// PurposeSynthesize merges several answers into one.
	PurposeSynthesize Purpose = "synthesize"

	// PurposeCompress summarises. Fast tier, cold, tightly bounded — a summary
	// longer than what it summarises has failed.
	PurposeCompress Purpose = "compress"

	// PurposeClassify answers a closed question. Deterministic and tiny; this
	// is the one that must never be allowed to write an essay.
	PurposeClassify Purpose = "classify"

	// PurposeReview comments on finished work. Critic tier.
	PurposeReview Purpose = "review"

	// PurposeAdjudicate settles a disagreement between models.
	PurposeAdjudicate Purpose = "adjudicate"
)

// policy is the tier and limits for a purpose.
type policy struct {
	tier      core.ModelTier
	maxTokens int
	temp      float64
}

// policies is the whole routing table, in one place, readable at a glance.
// That readability IS the feature: the previous spread of temperatures was
// impossible to review because no two of them were in the same file.
var policies = map[Purpose]policy{
	PurposePlan:       {core.ModelTierReasoning, 6000, 0.2},
	PurposeExecute:    {core.ModelTierCoding, 4000, 0.3},
	PurposeConverse:   {core.ModelTierCoding, 2000, 0.7},
	PurposeSynthesize: {core.ModelTierReasoning, 2000, 0.5},
	PurposeCompress:   {core.ModelTierFast, 1200, 0.1},
	PurposeClassify:   {core.ModelTierFast, 256, 0.0},
	PurposeReview:     {core.ModelTierCritic, 1500, 0.3},
	PurposeAdjudicate: {core.ModelTierReasoning, 1000, 0.2},
}

// defaultPolicy is used for an unknown purpose. Deliberately bounded rather
// than unlimited: an unrecognised purpose is a bug, and the safe failure is a
// short answer, not an expensive one.
var defaultPolicy = policy{core.ModelTierCoding, 2000, 0.3}

// Ask is one request to a model.
//
// There is no Model field and no Tier field. Those are the manager's decision;
// see the package comment.
type Ask struct {
	// Purpose decides tier, ceiling and temperature.
	Purpose Purpose

	// Messages is the prompt.
	Messages []core.Message

	// Complexity optionally informs routing within the tier. Zero is fine.
	Complexity int

	// Goal optionally describes the work, for routing and telemetry.
	Goal string

	// Tools offered for this call, if any.
	Tools []core.ToolSchema

	// MaxTokens overrides the purpose's ceiling. Use only where the purpose
	// genuinely cannot express the bound; it is never a way to remove one,
	// because a non-positive value falls back to the purpose default.
	MaxTokens int

	// Temperature overrides the purpose's temperature. Same caveat.
	Temperature *float64

	// Stream, when set, streams the reply through these callbacks.
	Stream *core.StreamCallbacks
}

// Answer is what came back.
type Answer struct {
	Text      string
	Model     string
	ToolCalls []core.ToolCall
	Raw       *core.CompletionResponse
}

// Router selects a client for a tier. Satisfied by router.Router; declared here
// so this package depends on the behaviour rather than the type.
type Router interface {
	Route(tier core.ModelTier, complexity int, desc string) (core.LLMClient, string, error)
}

// Manager routes model calls.
type Manager struct {
	router Router
}

// New returns a Manager over r. A nil router is refused rather than producing
// a manager whose every call fails at the point of use.
func New(r Router) (*Manager, error) {
	if r == nil {
		return nil, fmt.Errorf("modelport: nil router")
	}
	return &Manager{router: r}, nil
}

// PolicyFor exposes the tier and limits a purpose resolves to, for telemetry
// and for tests that assert the table rather than re-deriving it.
func PolicyFor(p Purpose) (tier core.ModelTier, maxTokens int, temperature float64) {
	pol, ok := policies[p]
	if !ok {
		pol = defaultPolicy
	}
	return pol.tier, pol.maxTokens, pol.temp
}

// Complete runs one model call.
//
// Every call leaves here with a token ceiling, which is the point: eight of
// the sites this replaces sent none, including the hottest three.
func (m *Manager) Complete(ctx context.Context, ask Ask) (*Answer, error) {
	if m == nil || m.router == nil {
		return nil, fmt.Errorf("modelport: no router")
	}
	if len(ask.Messages) == 0 {
		return nil, fmt.Errorf("modelport: %s call with no messages", ask.Purpose)
	}

	tier, maxTokens, temp := PolicyFor(ask.Purpose)
	if ask.MaxTokens > 0 {
		maxTokens = ask.MaxTokens
	}
	if ask.Temperature != nil {
		temp = *ask.Temperature
	}

	client, model, err := m.router.Route(tier, ask.Complexity, ask.Goal)
	if err != nil {
		return nil, fmt.Errorf("modelport: routing %s: %w", ask.Purpose, err)
	}
	if client == nil {
		return nil, fmt.Errorf("modelport: no model available for %s", ask.Purpose)
	}

	req := &core.CompletionRequest{
		Model:       model,
		Messages:    ask.Messages,
		Temperature: &temp,
		MaxTokens:   &maxTokens,
		Tools:       ask.Tools,
	}

	var resp *core.CompletionResponse
	if ask.Stream != nil {
		resp, err = client.ChatCompletionStream(ctx, req, ask.Stream)
	} else {
		resp, err = client.ChatCompletion(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("modelport: %s returned no choices", ask.Purpose)
	}

	choice := resp.Choices[0]
	return &Answer{
		Text:      choice.Message.Content,
		Model:     model,
		ToolCalls: choice.Message.ToolCalls,
		Raw:       resp,
	}, nil
}

// Embed produces a vector. Routed to the fast tier: embedding on a reasoning
// model costs more and returns the same vector shape.
func (m *Manager) Embed(ctx context.Context, text string) ([]float32, error) {
	if m == nil || m.router == nil {
		return nil, fmt.Errorf("modelport: no router")
	}
	client, _, err := m.router.Route(core.ModelTierFast, 0, "embedding")
	if err != nil || client == nil {
		return nil, fmt.Errorf("modelport: no model available for embedding: %w", err)
	}
	return client.CreateEmbedding(ctx, text)
}
