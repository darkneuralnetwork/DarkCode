package orchestrator

// testutil_test.go — shared test doubles for orchestrator package tests.
//
// Rather than mocking core.ModelRouter itself (Kernel.router is a concrete
// *router.Router, not an interface, so that's not an option), these helpers
// build a REAL router.Router wired to a fake core.LLMClient. This exercises
// the real tier-selection/fallback/consensus-fanout logic end-to-end with a
// deterministic, network-free client underneath — more realistic than a
// hand-rolled fake router, and it's the only boundary that's actually
// substitutable.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/router"
	"github.com/darkcode/memory/ctxengine"
	"github.com/darkcode/memory/memory"
	"github.com/darkcode/surfaces/ui"
	"github.com/darkcode/tools/tools"
)

// fakeLLMClient is a scripted core.LLMClient. Responses are dequeued one per
// call (the last one repeats once exhausted); respFunc, if set, overrides
// that and computes a response from the request instead.
type fakeLLMClient struct {
	name      string
	responses []string
	respFunc  func(callIndex int, req *core.CompletionRequest) string
	err       error         // if set, every call fails with this error
	delay     time.Duration // artificial per-call delay, for cancellation tests
	calls     int32         // atomic call counter
	// toolCallsFunc, if set, lets a test script a tool-using turn: the
	// returned calls (if non-empty) are attached to that call's response
	// message, driving the same ReAct act/observe loop a real tool-calling
	// model would. nil (the default) means no test relies on this — every
	// existing caller of fakeLLMClient is unaffected.
	toolCallsFunc func(callIndex int) []core.ToolCall
}

func (f *fakeLLMClient) nextContent(req *core.CompletionRequest) string {
	idx := int(atomic.AddInt32(&f.calls, 1)) - 1
	if f.respFunc != nil {
		return f.respFunc(idx, req)
	}
	if len(f.responses) == 0 {
		return "ok"
	}
	if idx < len(f.responses) {
		return f.responses[idx]
	}
	return f.responses[len(f.responses)-1]
}

func (f *fakeLLMClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	idx := int(atomic.LoadInt32(&f.calls))
	content := f.nextContent(req)
	var toolCalls []core.ToolCall
	if f.toolCallsFunc != nil {
		toolCalls = f.toolCallsFunc(idx)
	}
	return &core.CompletionResponse{
		Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: content, ToolCalls: toolCalls}}},
	}, nil
}

func (f *fakeLLMClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	resp, err := f.ChatCompletion(ctx, req)
	if err != nil {
		return nil, err
	}
	if cb != nil && cb.OnContent != nil && len(resp.Choices) > 0 {
		cb.OnContent(resp.Choices[0].Message.Content)
	}
	return resp, nil
}

func (f *fakeLLMClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (f *fakeLLMClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: f.name, Context: 8000}
}
func (f *fakeLLMClient) Ping(ctx context.Context) error { return nil }
func (f *fakeLLMClient) Close() error                   { return nil }

func (f *fakeLLMClient) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

// newTestRouter builds a real router.Router with client registered across
// every tier the kernel's execution paths might route to, so tests don't
// need to know which specific tier a given code path happens to use.
func newTestRouter(mode core.RoutingMode, client core.LLMClient, modelName string) *router.Router {
	r := router.NewRouter(mode, nil)
	for _, tier := range []core.ModelTier{
		core.ModelTierReasoning, core.ModelTierCoding, core.ModelTierFast,
		core.ModelTierLocal, core.ModelTierCritic,
	} {
		r.RegisterModel(tier, client, modelName)
	}
	r.MarkPrimary(modelName)
	return r
}

// testKernelDeps bundles everything newTestKernel builds, for tests that
// need to reach into a specific piece (e.g. the registry, to register a
// fake tool).
type testKernelDeps struct {
	Kernel   *Kernel
	Router   *router.Router
	Registry *tools.Registry
	Memory   *memory.System
	Client   *fakeLLMClient
	Emitter  *ui.EventEmitter
}

// newTestKernel builds a fully-wired Kernel backed by real memory/registry/
// compressor and a single fake LLM client registered across all tiers, in
// single-routing mode. Use newTestKernelConsensus for consensus-mode tests.
func newTestKernel(t *testing.T, client *fakeLLMClient) *testKernelDeps {
	t.Helper()
	return newTestKernelWithMode(t, core.RouteSingle, client)
}

func newTestKernelWithMode(t *testing.T, mode core.RoutingMode, client *fakeLLMClient) *testKernelDeps {
	t.Helper()
	if client == nil {
		client = &fakeLLMClient{name: "fake-primary"}
	}
	reg := tools.NewRegistry()
	mem, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("memory.NewSystem: %v", err)
	}
	t.Cleanup(mem.Shutdown)

	rtr := newTestRouter(mode, client, client.name)
	comp := ctxengine.NewEngine(nil)
	comp.SetClient(client, client.name)
	comp.SetRouter(rtr)

	k := New(DefaultConfig(), rtr, reg, mem, comp, nil)

	return &testKernelDeps{Kernel: k, Router: rtr, Registry: reg, Memory: mem, Client: client}
}

// mustOverride adapts ApplyRequestOverrides for the tests that only assert on
// shared router/gate state (mode, safety, brain). Those settings still live on
// the router and gate, so the returned context carries nothing they need and
// discarding it is correct. Tests about the per-request flags must NOT use
// this — they need the context. See override_isolation_test.go.
func mustOverride(k *Kernel, mode, safety, loop, tools, brain string) func() {
	_, restore := k.ApplyRequestOverrides(context.Background(), mode, safety, loop, tools, brain)
	return restore
}
