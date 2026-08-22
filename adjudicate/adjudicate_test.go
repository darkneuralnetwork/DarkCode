package adjudicate

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/datasource"
	"github.com/darkcode/modelport"
)

// ---- fakes -----------------------------------------------------------------

type fakeClient struct {
	mu    sync.Mutex
	calls int
	err   error
	// inspect sees every request, so a test can assert on what was sent.
	inspect func(*core.CompletionRequest)
	reply   string
}

func (c *fakeClient) ChatCompletion(_ context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.inspect != nil {
		c.inspect(req)
	}
	if c.err != nil {
		return nil, c.err
	}
	reply := c.reply
	if reply == "" {
		reply = "reply"
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Content: reply}}}}, nil
}

func (c *fakeClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, _ *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return c.ChatCompletion(ctx, req)
}
func (c *fakeClient) CreateEmbedding(context.Context, string) ([]float32, error) { return nil, nil }
func (c *fakeClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "fake", Context: 32000}
}
func (c *fakeClient) Ping(context.Context) error { return nil }
func (c *fakeClient) Close() error               { return nil }
func (c *fakeClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type fakeRouter struct{ c *fakeClient }

func (r fakeRouter) Route(core.ModelTier, int, string) (core.LLMClient, string, error) {
	return r.c, "fake", nil
}

// silentEvidence is a store with no graph: checking is impossible, which is the
// only branch that may reach a debate.
type silentEvidence struct{}

func (silentEvidence) Adjudicate([]string) (int, []datasource.Support, bool) {
	return -1, nil, false
}

type recorder struct{ n int }

func (r *recorder) Critiqued(_, _, _, body string) {
	if body != "" {
		r.n++
	}
}

func newAdj(t *testing.T, c *fakeClient, opts ...Option) *Adjudicator {
	t.Helper()
	m, err := modelport.New(fakeRouter{c})
	if err != nil {
		t.Fatal(err)
	}
	return New(m, silentEvidence{}, opts...)
}

func twoWayConflict() *core.ConsensusResult {
	return &core.ConsensusResult{
		Synthesized: "a merged answer",
		Conflict:    true,
		Contributions: []core.ModelContribution{
			{Model: "model-a", Role: "analyst", Output: "the timeout is 30 seconds"},
			{Model: "model-b", Role: "skeptic", Output: "no, the timeout is 5 seconds"},
		},
	}
}

// ---- tests -----------------------------------------------------------------

// TestDebateIsOffByDefault. Two extra calls on a metered tier for something
// nobody asked for is exactly the cost this design exists to avoid.
func TestDebateIsOffByDefault(t *testing.T) {
	c := &fakeClient{}
	res := newAdj(t, c).Verdict(context.Background(), "how long is the timeout", twoWayConflict())
	if res.Debated {
		t.Error("debate ran while disabled")
	}
	if c.count() != 0 {
		t.Errorf("debate spent %d call(s) while disabled", c.count())
	}
	if res.Answer != "a merged answer" {
		t.Errorf("answer = %q, want the synthesis", res.Answer)
	}
}

// TestDebateNeedsTwoRealPositions — one model, or one that errored, is not a
// disagreement.
func TestDebateNeedsTwoRealPositions(t *testing.T) {
	cases := map[string]*core.ConsensusResult{
		"single contribution": {Conflict: true, Synthesized: "s", Contributions: []core.ModelContribution{
			{Model: "a", Output: "only answer"}}},
		"one errored": {Conflict: true, Synthesized: "s", Contributions: []core.ModelContribution{
			{Model: "a", Output: "answer"}, {Model: "b", Error: "429 rate limited"}}},
		"one empty": {Conflict: true, Synthesized: "s", Contributions: []core.ModelContribution{
			{Model: "a", Output: "answer"}, {Model: "b", Output: "   "}}},
	}
	for name, cr := range cases {
		t.Run(name, func(t *testing.T) {
			c := &fakeClient{}
			adj := newAdj(t, c, WithDebate(func() bool { return true }))
			if res := adj.Verdict(context.Background(), "q", cr); res.Debated {
				t.Error("debated without two usable positions")
			}
			if c.count() != 0 {
				t.Errorf("spent %d call(s) on a non-disagreement", c.count())
			}
		})
	}
}

// TestDebateRunsExactlyOneRound. Accuracy plateaus at two or three rounds and
// drift compounds per round, so the cap is the design rather than a limitation.
func TestDebateRunsExactlyOneRound(t *testing.T) {
	c := &fakeClient{reply: "settled"}
	adj := newAdj(t, c, WithDebate(func() bool { return true }))

	res := adj.Verdict(context.Background(), "how long is the timeout", twoWayConflict())
	if !res.Debated {
		t.Fatal("no exchange ran")
	}
	// Two critiques and one settlement. A second round would be five.
	if c.count() != 3 {
		t.Errorf("exchange spent %d call(s), want 3 (two critiques + one settlement)", c.count())
	}
	if res.Transcript == "" {
		t.Error("the exchange left no transcript")
	}
}

// TestDebateAnchorsOnTheOriginalQuestion. Re-pinning the goal in every prompt is
// the published mitigation for problem drift, and the reason one round is enough
// rather than merely affordable.
func TestDebateAnchorsOnTheOriginalQuestion(t *testing.T) {
	const goal = "how long is the auth timeout"
	seen := 0
	c := &fakeClient{reply: "x", inspect: func(req *core.CompletionRequest) {
		for _, m := range req.Messages {
			if strings.Contains(m.ContentString(), goal) {
				seen++
				break
			}
		}
	}}
	adj := newAdj(t, c, WithDebate(func() bool { return true }))
	adj.Verdict(context.Background(), goal, twoWayConflict())
	if seen < 3 {
		t.Errorf("the question was re-pinned in only %d of 3 prompts", seen)
	}
}

// TestDebateSurvivesAnUnreachableModel. A failed exchange must fall back to the
// synthesis, not lose the answer.
func TestDebateSurvivesAnUnreachableModel(t *testing.T) {
	c := &fakeClient{err: context.DeadlineExceeded}
	adj := newAdj(t, c, WithDebate(func() bool { return true }))

	res := adj.Verdict(context.Background(), "q", twoWayConflict())
	if res.Debated {
		t.Error("reported a run that produced nothing")
	}
	if res.Answer != "a merged answer" {
		t.Errorf("answer = %q, want the synthesis to survive a failed exchange", res.Answer)
	}
}

// TestDebateRecordsTheExchange — the exchange being inspectable is what makes it
// auditable rather than a black box.
func TestDebateRecordsTheExchange(t *testing.T) {
	c := &fakeClient{reply: "crit"}
	rec := &recorder{}
	adj := newAdj(t, c, WithDebate(func() bool { return true }), WithRecorder(rec))

	adj.Verdict(context.Background(), "q", twoWayConflict())
	if rec.n != 2 {
		t.Errorf("recorded %d critique(s), want 2 (one per direction)", rec.n)
	}
}

// TestNilConsensusIsNotADereference — the caller passes whatever the router
// returned, and that used to be dereferenced on the nil branch.
func TestNilConsensusIsNotADereference(t *testing.T) {
	if res := newAdj(t, &fakeClient{}).Verdict(context.Background(), "q", nil); res.Answer != "" {
		t.Errorf("answer = %q for a nil consensus", res.Answer)
	}
}
