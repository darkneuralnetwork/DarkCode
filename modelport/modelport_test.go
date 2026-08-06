package modelport

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

type fakeClient struct {
	got    *core.CompletionRequest
	reply  string
	err    error
	window int
}

func (f *fakeClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{
		Message: core.ResponseMessage{Role: "assistant", Content: f.reply},
	}}}, nil
}
func (f *fakeClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return f.ChatCompletion(ctx, req)
}
func (f *fakeClient) CreateEmbedding(context.Context, string) ([]float32, error) {
	return []float32{1, 2, 3}, nil
}
func (f *fakeClient) ModelInfo() core.ModelMetadata { return core.ModelMetadata{Context: f.window} }
func (f *fakeClient) Ping(context.Context) error    { return nil }
func (f *fakeClient) Close() error                  { return nil }

type fakeRouter struct {
	client   *fakeClient
	gotTier  core.ModelTier
	routeErr error
}

func (r *fakeRouter) Route(tier core.ModelTier, complexity int, desc string) (core.LLMClient, string, error) {
	r.gotTier = tier
	if r.routeErr != nil {
		return nil, "", r.routeErr
	}
	return r.client, "fake-model", nil
}

func newManager(t *testing.T) (*Manager, *fakeRouter, *fakeClient) {
	t.Helper()
	c := &fakeClient{reply: "ok"}
	r := &fakeRouter{client: c}
	m, err := New(r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, r, c
}

var msgs = []core.Message{{Role: core.RoleUser, Content: "hello"}}

// TestEveryCallGetsATokenCeiling is the regression, and the expensive one.
// Eight of the eighteen completion sites sent no MaxTokens, so their ceiling
// was whatever the provider defaults to — usually the rest of the context
// window. They included the ReAct loop's main call, every sub-agent worker
// turn, and every conversational answer.
func TestEveryCallGetsATokenCeiling(t *testing.T) {
	for _, p := range []Purpose{
		PurposePlan, PurposeExecute, PurposeConverse, PurposeSynthesize,
		PurposeCompress, PurposeClassify, PurposeReview, PurposeAdjudicate,
		Purpose("something nobody defined"),
	} {
		m, _, c := newManager(t)
		if _, err := m.Complete(context.Background(), Ask{Purpose: p, Messages: msgs}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if c.got.MaxTokens == nil {
			t.Errorf("%s sent no token ceiling — the reply is bounded only by the context window", p)
			continue
		}
		if *c.got.MaxTokens <= 0 {
			t.Errorf("%s sent a ceiling of %d", p, *c.got.MaxTokens)
		}
	}
}

// TestUnknownPurposeIsBoundedNotUnlimited — an unrecognised purpose is a bug,
// and the safe failure is a short answer rather than an expensive one.
func TestUnknownPurposeIsBoundedNotUnlimited(t *testing.T) {
	_, max, _ := PolicyFor(Purpose("typo"))
	if max <= 0 || max > 4000 {
		t.Errorf("unknown purpose resolved to a %d-token ceiling", max)
	}
}

// TestPurposeDecidesTheTier — the whole point of naming a purpose instead of a
// model is that routing lives in one table.
func TestPurposeDecidesTheTier(t *testing.T) {
	// With local preference off, an auxiliary purpose falls through its local
	// rungs to the cloud tier at the end of its ladder.
	cases := map[Purpose]core.ModelTier{
		PurposePlan:       core.ModelTierReasoning,
		PurposeExecute:    core.ModelTierCoding,
		PurposeCompress:   core.ModelTierFast,
		PurposeClassify:   core.ModelTierFast,
		PurposeReview:     core.ModelTierCritic,
		PurposeAdjudicate: core.ModelTierReasoning,
	}
	for p, want := range cases {
		m, r, _ := newManager(t)
		if _, err := m.Complete(context.Background(), Ask{Purpose: p, Messages: msgs}); err != nil {
			t.Fatal(err)
		}
		if r.gotTier != want {
			t.Errorf("%s routed to %v, want %v", p, r.gotTier, want)
		}
	}
}

// TestClassifyCannotWriteAnEssay — a closed question answered at length is a
// classifier that has stopped classifying.
func TestClassifyCannotWriteAnEssay(t *testing.T) {
	_, max, temp := PolicyFor(PurposeClassify)
	if max > 512 {
		t.Errorf("classify ceiling is %d tokens", max)
	}
	if temp != 0 {
		t.Errorf("classify temperature is %v, want 0 — a classifier must be deterministic", temp)
	}
}

// TestCompressIsBoundedBelowWhatItSummarises.
func TestCompressIsBoundedBelowWhatItSummarises(t *testing.T) {
	_, compressMax, _ := PolicyFor(PurposeCompress)
	_, planMax, _ := PolicyFor(PurposePlan)
	if compressMax >= planMax {
		t.Errorf("compress may emit %d tokens against a plan's %d — a summary that long has failed",
			compressMax, planMax)
	}
}

func TestExplicitOverridesWin(t *testing.T) {
	m, _, c := newManager(t)
	temp := 0.9
	if _, err := m.Complete(context.Background(), Ask{
		Purpose: PurposeClassify, Messages: msgs, MaxTokens: 99, Temperature: &temp,
	}); err != nil {
		t.Fatal(err)
	}
	if *c.got.MaxTokens != 99 {
		t.Errorf("MaxTokens = %d, want the explicit 99", *c.got.MaxTokens)
	}
	if *c.got.Temperature != 0.9 {
		t.Errorf("Temperature = %v, want the explicit 0.9", *c.got.Temperature)
	}
}

// TestOverrideCannotRemoveTheCeiling — an override is for expressing a
// different bound, never for removing one.
func TestOverrideCannotRemoveTheCeiling(t *testing.T) {
	for _, bad := range []int{0, -1, -1000} {
		m, _, c := newManager(t)
		if _, err := m.Complete(context.Background(), Ask{
			Purpose: PurposeCompress, Messages: msgs, MaxTokens: bad,
		}); err != nil {
			t.Fatal(err)
		}
		if c.got.MaxTokens == nil || *c.got.MaxTokens <= 0 {
			t.Errorf("MaxTokens: %v removed the ceiling", bad)
		}
	}
}

func TestEmptyAskIsRefused(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposePlan}); err == nil {
		t.Error("a call with no messages was sent to a model")
	}
}

func TestRoutingFailureIsReported(t *testing.T) {
	c := &fakeClient{}
	r := &fakeRouter{client: c, routeErr: errors.New("no models registered")}
	m, _ := New(r)
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposePlan, Messages: msgs}); err == nil {
		t.Error("a routing failure was swallowed")
	}
}

func TestNilRouterIsRefused(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("New(nil) returned a manager whose every call fails at the point of use")
	}
}

func TestStreamingUsesTheSamePolicy(t *testing.T) {
	m, r, c := newManager(t)
	if _, err := m.Complete(context.Background(), Ask{
		Purpose: PurposeConverse, Messages: msgs, Stream: &core.StreamCallbacks{},
	}); err != nil {
		t.Fatal(err)
	}
	if c.got.MaxTokens == nil {
		t.Error("a streamed call sent no ceiling")
	}
	if r.gotTier != core.ModelTierCoding {
		t.Errorf("streamed call routed to %v", r.gotTier)
	}
}

// TestEmbedUsesTheFastTier — embedding on a reasoning model costs more and
// returns the same vector shape.
func TestEmbedUsesTheFastTier(t *testing.T) {
	m, r, _ := newManager(t)
	if _, err := m.Embed(context.Background(), "text"); err != nil {
		t.Fatal(err)
	}
	if r.gotTier != core.ModelTierFast {
		t.Errorf("Embed routed to %v, want the fast tier", r.gotTier)
	}
}

// ── the auxiliary ladder ─────────────────────────────────────────────────────

// ladderRouter records every tier asked for and serves only the ones listed.
type ladderRouter struct {
	available map[core.ModelTier]int // tier -> context window
	asked     []core.ModelTier
	client    *fakeClient
}

func (r *ladderRouter) Route(tier core.ModelTier, complexity int, desc string) (core.LLMClient, string, error) {
	r.asked = append(r.asked, tier)
	w, ok := r.available[tier]
	if !ok {
		return nil, "", errors.New("no model at this tier")
	}
	c := &fakeClient{reply: "ok", window: w}
	r.client = c
	return c, string(tier), nil
}

// TestAuxiliaryWorkPrefersLocal is the regression this ladder exists for.
//
// The kernel had a second routing policy — RouteAux — that sent auxiliary work
// to a local model, free, before falling back to cloud. A single tier per
// purpose could not express it, so routing compress to ModelTierFast would
// have quietly moved that work onto a metered model: a boundary count
// improving while the tool got more expensive.
func TestAuxiliaryWorkPrefersLocal(t *testing.T) {
	r := &ladderRouter{available: map[core.ModelTier]int{
		core.ModelTierMediumLocal: 32000,
		core.ModelTierFast:        128000,
	}}
	m, _ := New(r)
	m.PreferLocal(true)

	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposeCompress, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	if r.asked[0] != core.ModelTierMediumLocal {
		t.Errorf("compress asked for %v first, want the local tier", r.asked[0])
	}
}

// TestTheUsersWorkIsNeverDemotedToLocal — a worse plan costs more than the
// tokens it saved.
func TestTheUsersWorkIsNeverDemotedToLocal(t *testing.T) {
	for _, p := range []Purpose{PurposePlan, PurposeExecute, PurposeConverse, PurposeSynthesize, PurposeAdjudicate} {
		if IsAuxiliary(p) {
			t.Errorf("%s is on the auxiliary ladder — it would run on a small local model", p)
		}
		for _, tier := range TiersFor(p) {
			if isLocal(tier) {
				t.Errorf("%s may route to the local tier %v", p, tier)
			}
		}
	}
}

// TestLocalIsSkippedWhenThePromptWouldOverflow — the check that made
// local-first safe. A big prompt on a small local model does not save money,
// it fails, and then the work is done twice.
func TestLocalIsSkippedWhenThePromptWouldOverflow(t *testing.T) {
	r := &ladderRouter{available: map[core.ModelTier]int{
		core.ModelTierMediumLocal: 100, // tiny window
		core.ModelTierFast:        128000,
	}}
	m, _ := New(r)
	m.PreferLocal(true)

	big := []core.Message{{Role: core.RoleUser, Content: strings.Repeat("word ", 5000)}}
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposeCompress, Messages: big}); err != nil {
		t.Fatal(err)
	}
	last := r.asked[len(r.asked)-1]
	if isLocal(last) {
		t.Errorf("a prompt too big for the local window was sent to %v anyway", last)
	}
}

// TestPreferLocalOffHonoursTheSetting — running locally anyway would be the
// manager overriding a choice the user made.
func TestPreferLocalOffHonoursTheSetting(t *testing.T) {
	r := &ladderRouter{available: map[core.ModelTier]int{
		core.ModelTierMediumLocal: 32000,
		core.ModelTierFast:        128000,
	}}
	m, _ := New(r)
	m.PreferLocal(false)

	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposeCompress, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	for _, tier := range r.asked {
		if isLocal(tier) {
			t.Errorf("local tier %v was tried with the preference off", tier)
		}
	}
}

// TestLadderFallsThroughToCloud — a machine with no local model must still work.
func TestLadderFallsThroughToCloud(t *testing.T) {
	r := &ladderRouter{available: map[core.ModelTier]int{core.ModelTierFast: 128000}}
	m, _ := New(r)
	m.PreferLocal(true)

	ans, err := m.Complete(context.Background(), Ask{Purpose: PurposeClassify, Messages: msgs})
	if err != nil {
		t.Fatalf("no local model available and the call failed: %v", err)
	}
	if ans.Model != string(core.ModelTierFast) {
		t.Errorf("landed on %q, want the cloud fallback", ans.Model)
	}
}

func TestNoModelAnywhereIsAnError(t *testing.T) {
	r := &ladderRouter{available: map[core.ModelTier]int{}}
	m, _ := New(r)
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposePlan, Messages: msgs}); err == nil {
		t.Error("a call with no model anywhere reported success")
	}
}

// overflowClient fails the first call with a context overflow, then succeeds.
type overflowClient struct {
	calls    int
	window   int
	gotSizes []int // estimated tokens per dispatch
}

func (o *overflowClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	o.calls++
	o.gotSizes = append(o.gotSizes, core.EstimateTokens(messagesText(req.Messages)))
	if o.calls == 1 {
		return nil, core.ErrContextTooLong
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{
		Message: core.ResponseMessage{Role: "assistant", Content: "recovered"},
	}}}, nil
}
func (o *overflowClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return o.ChatCompletion(ctx, req)
}
func (o *overflowClient) CreateEmbedding(context.Context, string) ([]float32, error) { return nil, nil }
func (o *overflowClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{Context: o.window}
}
func (o *overflowClient) Ping(context.Context) error { return nil }
func (o *overflowClient) Close() error               { return nil }

type oneClientRouter struct{ c core.LLMClient }

func (r *oneClientRouter) Route(core.ModelTier, int, string) (core.LLMClient, string, error) {
	return r.c, "m", nil
}

// TestContextOverflowIsRetriedOnce — the fit uses a token ESTIMATE, and
// estimates drift from the model's real tokenizer. Aborting on that loses a
// whole task for a counting error.
func TestContextOverflowIsRetriedOnce(t *testing.T) {
	oc := &overflowClient{window: 8000}
	m, _ := New(&oneClientRouter{c: oc})

	big := make([]core.Message, 0, 60)
	for i := 0; i < 60; i++ {
		big = append(big, core.Message{Role: core.RoleUser, Content: strings.Repeat("word ", 200)})
	}

	ans, err := m.Complete(context.Background(), Ask{Purpose: PurposeExecute, Messages: big})
	if err != nil {
		t.Fatalf("an overflow was not recovered: %v", err)
	}
	if ans.Text != "recovered" {
		t.Errorf("output = %q", ans.Text)
	}
	if oc.calls != 2 {
		t.Errorf("dispatched %d times, want 2 (one overflow, one retry)", oc.calls)
	}
	if len(oc.gotSizes) == 2 && oc.gotSizes[1] >= oc.gotSizes[0] {
		t.Errorf("the retry sent %d tokens after %d — shrinking did nothing, so the "+
			"retry can only fail the same way", oc.gotSizes[1], oc.gotSizes[0])
	}
}

// alwaysOverflow never recovers.
type alwaysOverflow struct{ calls int }

func (a *alwaysOverflow) ChatCompletion(context.Context, *core.CompletionRequest) (*core.CompletionResponse, error) {
	a.calls++
	return nil, core.ErrContextTooLong
}
func (a *alwaysOverflow) ChatCompletionStream(ctx context.Context, r *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return a.ChatCompletion(ctx, r)
}
func (a *alwaysOverflow) CreateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (a *alwaysOverflow) ModelInfo() core.ModelMetadata { return core.ModelMetadata{Context: 8000} }
func (a *alwaysOverflow) Ping(context.Context) error    { return nil }
func (a *alwaysOverflow) Close() error                  { return nil }

// TestOverflowIsRetriedOnceNotForever — a second overflow is not tokenizer
// drift, it is a prompt that genuinely does not fit.
func TestOverflowIsRetriedOnceNotForever(t *testing.T) {
	ao := &alwaysOverflow{}
	m, _ := New(&oneClientRouter{c: ao})
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposeExecute, Messages: msgs}); err == nil {
		t.Error("a prompt that never fits reported success")
	}
	if ao.calls > 2 {
		t.Errorf("dispatched %d times — the retry is not bounded", ao.calls)
	}
}

// TestOtherErrorsAreNotRetried — retrying an auth failure spends a second call
// to be told the same thing.
func TestOtherErrorsAreNotRetried(t *testing.T) {
	c := &fakeClient{err: errors.New("401 unauthorized"), window: 8000}
	m, _ := New(&oneClientRouter{c: c})
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposeExecute, Messages: msgs}); err == nil {
		t.Fatal("expected the error to surface")
	}
}
