package modelport

import (
	"context"
	"errors"
	"testing"

	"github.com/darkcode/core"
)

type fakeClient struct {
	got   *core.CompletionRequest
	reply string
	err   error
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
func (f *fakeClient) ModelInfo() core.ModelMetadata { return core.ModelMetadata{} }
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
