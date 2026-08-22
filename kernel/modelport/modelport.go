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
	"errors"
	"fmt"
	"strings"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/ctxfit"
	"github.com/darkcode/model/llm"
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

// policy is the tier preference and limits for a purpose.
//
// tiers is ORDERED, and that ordering is where local-first lives. The kernel
// had a second routing policy for this — RouteAux, which tried a medium-local
// then a tiny-local model before falling back to cloud, so auxiliary work ran
// free. A single tier per purpose could not express it, and routing compress
// or classify to ModelTierFast would have moved that work off local models
// onto metered ones: a boundary count improving while the tool got more
// expensive. Absorbing it here leaves one routing decision instead of two, and
// extends local-first from the three sites that used RouteAux to every
// auxiliary call.
type policy struct {
	tiers     []core.ModelTier
	maxTokens int
	temp      float64
}

// policies is the whole routing table, in one place, readable at a glance.
// That readability IS the feature: the previous spread of temperatures was
// impossible to review because no two of them were in the same file.
// aux is the tier order for work the user did not ask for: summarising,
// classifying, reviewing. It runs on whatever is cheapest that can do the job,
// and a local model is free.
var aux = []core.ModelTier{core.ModelTierMediumLocal, core.ModelTierTinyLocal, core.ModelTierFast}

var policies = map[Purpose]policy{
	// The user's work. Never demoted to a local model to save money — a worse
	// plan costs more than the tokens it saved.
	PurposePlan:       {[]core.ModelTier{core.ModelTierReasoning}, 6000, 0.2},
	PurposeExecute:    {[]core.ModelTier{core.ModelTierCoding}, 4000, 0.3},
	PurposeConverse:   {[]core.ModelTier{core.ModelTierCoding}, 2000, 0.7},
	PurposeSynthesize: {[]core.ModelTier{core.ModelTierReasoning}, 2000, 0.5},

	// Adjudication decides which of two models was right. A weaker model
	// settling that is worse than either candidate, so it is not auxiliary
	// however cheap it looks.
	PurposeAdjudicate: {[]core.ModelTier{core.ModelTierReasoning}, 1000, 0.2},

	// Work about the work. Local first: summarising and answering a closed
	// question are what small models are good at, and they are free.
	PurposeCompress: {aux, 1200, 0.1},
	PurposeClassify: {aux, 256, 0.0},

	// Review wants the critic model when there is one, but it is still
	// advisory, so it degrades to the auxiliary ladder rather than to the
	// primary.
	PurposeReview: {append([]core.ModelTier{core.ModelTierCritic}, aux...), 1500, 0.3},
}

// defaultPolicy is used for an unknown purpose. Deliberately bounded rather
// than unlimited: an unrecognised purpose is a bug, and the safe failure is a
// short answer, not an expensive one.
var defaultPolicy = policy{[]core.ModelTier{core.ModelTierCoding}, 2000, 0.3}

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
	// preferLocal mirrors the config's use_local_for_aux. When false, local
	// rungs are skipped and auxiliary work goes to the cloud tier — the user
	// asked for that, and silently running it locally anyway would be the
	// manager overriding a setting.
	preferLocal bool
}

// PreferLocal enables the local rungs of the auxiliary ladder.
func (m *Manager) PreferLocal(on bool) { m.preferLocal = on }

// New returns a Manager over r. A nil router is accepted: such a Manager
// still works for CompleteWith (which never routes — the caller supplies the
// client), and correctly refuses at the point of use for Complete (which
// does). This lets a caller that has no router of its own — planwork.Amend
// deliberately doesn't construct or route to a client — still get the
// shared ceiling/temperature policy, window-fit and dispatch machinery for
// a client someone else already selected.
func New(r Router) (*Manager, error) {
	return &Manager{router: r}, nil
}

// PolicyFor exposes the tier and limits a purpose resolves to, for telemetry
// and for tests that assert the table rather than re-deriving it.
func PolicyFor(p Purpose) (tier core.ModelTier, maxTokens int, temperature float64) {
	pol := policyFor(p)
	return pol.tiers[0], pol.maxTokens, pol.temp
}

// TiersFor exposes the ordered tier preference, so telemetry can say which
// ladder a call climbed rather than only where it landed.
func TiersFor(p Purpose) []core.ModelTier {
	return append([]core.ModelTier(nil), policyFor(p).tiers...)
}

func policyFor(p Purpose) policy {
	if pol, ok := policies[p]; ok && len(pol.tiers) > 0 {
		return pol
	}
	return defaultPolicy
}

// IsAuxiliary reports whether a purpose is work ABOUT the work rather than the
// work itself. Auxiliary calls prefer a local model.
func IsAuxiliary(p Purpose) bool {
	tiers := policyFor(p).tiers
	for _, t := range tiers {
		if t == core.ModelTierMediumLocal || t == core.ModelTierTinyLocal {
			return true
		}
	}
	return false
}

// Complete runs one model call, routing to a client itself.
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

	pol := policyFor(ask.Purpose)
	client, model, err := m.route(pol, ask)
	if err != nil {
		return nil, err
	}
	return m.dispatch(ctx, client, model, pol, ask)
}

// CompleteWith runs one model call against a client the caller already
// chose, rather than one Complete would route to itself.
//
// Some callers can't be expressed as "route by tier": a consensus fan-out
// hits every registered model in one logical operation, not one; a DAG
// worker needs slot-aware routing so concurrent workers in one wave land on
// different models; a compression path may already prefer a specific local
// client. Those callers still want the same ceiling/temperature policy,
// window-fit, Purpose-tagged dispatch and retry/overflow handling Complete
// gives everyone else — they just need to supply the client themselves.
func (m *Manager) CompleteWith(ctx context.Context, client core.LLMClient, model string, ask Ask) (*Answer, error) {
	if m == nil {
		return nil, fmt.Errorf("modelport: nil manager")
	}
	if client == nil {
		return nil, fmt.Errorf("modelport: nil client")
	}
	if len(ask.Messages) == 0 {
		return nil, fmt.Errorf("modelport: %s call with no messages", ask.Purpose)
	}
	return m.dispatch(ctx, client, model, policyFor(ask.Purpose), ask)
}

// dispatch is the shared tail of Complete and CompleteWith: resolve the
// policy's ceiling/temperature (with Ask overrides), fit to the given
// client's window, build the request, and send it.
func (m *Manager) dispatch(ctx context.Context, client core.LLMClient, model string, pol policy, ask Ask) (*Answer, error) {
	maxTokens, temp := pol.maxTokens, pol.temp
	if ask.MaxTokens > 0 {
		maxTokens = ask.MaxTokens
	}
	if ask.Temperature != nil {
		temp = *ask.Temperature
	}

	// Fit to the window of the model actually chosen. Every caller used to do
	// this itself, which means every caller had to remember — and the one that
	// forgets overflows. Doing it here makes "never sent more than it can
	// take" a property of the manager rather than of each call site, and it is
	// more accurate besides: the fit happens after routing, against the model
	// that will really answer, not the one the caller guessed.
	messages := ctxfit.FitClient(ask.Messages, client, 0, len(ask.Tools))

	req := &core.CompletionRequest{
		Model:       model,
		Messages:    messages,
		Temperature: &temp,
		MaxTokens:   &maxTokens,
		Tools:       ask.Tools,
	}

	resp, err := m.send(ctx, client, req, ask)
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

// overflowRefitFraction is how much of what was ACTUALLY SENT a retry is
// squeezed into.
//
// Deliberately relative to the prompt, not to the window. Refitting to a
// fraction of the window is a no-op whenever the estimate already fit — which
// is exactly the case this recovers from, because the provider only disagreed
// after we decided the prompt fit. A retry that sends the same bytes can only
// fail the same way; a test caught this doing precisely that.
const overflowRefitFraction = 75

// send dispatches, and retries once on a context overflow.
//
// The fit above uses a token ESTIMATE, and estimates drift from the model's
// real tokenizer. When the provider disagrees, shrinking hard and trying the
// same call again recovers the turn; aborting loses the whole task for a
// counting error. Retried once only — a second overflow is not drift, it is a
// prompt that genuinely does not fit.
func (m *Manager) send(ctx context.Context, client core.LLMClient, req *core.CompletionRequest, ask Ask) (*core.CompletionResponse, error) {
	// Tags the call log (model/llm/call_log.go) with which subsystem this
	// request is for, so "how many real requests did we make" is answerable
	// per-purpose ("execute" vs "compress" vs "plan"), not just in aggregate.
	ctx = llm.WithPurpose(ctx, string(ask.Purpose))
	dispatch := func() (*core.CompletionResponse, error) {
		if ask.Stream != nil {
			return client.ChatCompletionStream(ctx, req, ask.Stream)
		}
		return client.ChatCompletion(ctx, req)
	}

	resp, err := dispatch()
	if err == nil || !errors.Is(err, core.ErrContextTooLong) {
		return resp, err
	}

	sent := core.EstimateTokens(messagesText(req.Messages))
	target := sent * overflowRefitFraction / 100
	// Never shrink past the model's own window either, in case the estimate
	// was wrong in the other direction.
	if w := client.ModelInfo().Context; w > 0 && target > w*overflowRefitFraction/100 {
		target = w * overflowRefitFraction / 100
	}
	if target <= 0 {
		return nil, err
	}
	req.Messages = ctxfit.FitToWindow(req.Messages, target, 0)
	return dispatch()
}

// route climbs the purpose's tier preference and returns the first model that
// can take the call.
//
// A local tier is skipped when the prompt does not fit its window. That check
// is what made local-first safe in the policy this replaced: a big auxiliary
// prompt sent to a small local model does not save money, it overflows and
// fails, and then the work is done twice.
func (m *Manager) route(pol policy, ask Ask) (core.LLMClient, string, error) {
	promptTokens := core.EstimateTokens(messagesText(ask.Messages))

	var lastErr error
	for _, tier := range pol.tiers {
		if isLocal(tier) && !m.preferLocal {
			continue // the user asked for cloud; honour it
		}
		c, model, err := m.router.Route(tier, ask.Complexity, ask.Goal)
		if err != nil || c == nil {
			lastErr = err
			continue
		}
		if isLocal(tier) {
			if w := c.ModelInfo().Context; w > 0 && promptTokens > w {
				continue // would overflow; try the next rung
			}
		}
		return c, model, nil
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("modelport: routing %s: %w", ask.Purpose, lastErr)
	}
	return nil, "", fmt.Errorf("modelport: no model available for %s", ask.Purpose)
}

func isLocal(t core.ModelTier) bool {
	return t == core.ModelTierMediumLocal || t == core.ModelTierTinyLocal || t == core.ModelTierLocal
}

func messagesText(msgs []core.Message) string {
	var b strings.Builder
	for _, msg := range msgs {
		b.WriteString(msg.ContentString())
		b.WriteByte('\n')
	}
	return b.String()
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
