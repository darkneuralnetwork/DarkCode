package agents

// subagent_completewith_test.go — regression coverage for migrating
// SubAgent.Execute's completion call onto modelport.CompleteWith. Two things
// were at risk in that migration and had no existing test locking them down:
// the request now carries PurposeExecute's shared ceiling (it sent none
// before), and the errMgr-driven retry-with-modified-history path (the fix
// for Gemini's thought_signature/INVALID_ARGUMENT error — see
// orchestrator.ErrorManager) still fires correctly now that the completion
// call goes through CompleteWith instead of a raw client call.

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/modelport"
	"github.com/darkcode/tools/tools"
)

// reqCapturingClient records the request from every call, and can be primed
// to fail its first N calls with a fixed error before succeeding.
type reqCapturingClient struct {
	failFirstN int
	failErr    error
	calls      int
	got        []*core.CompletionRequest
}

func (c *reqCapturingClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return c.ChatCompletionStream(ctx, req, nil)
}
func (c *reqCapturingClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	c.got = append(c.got, req)
	c.calls++
	if c.calls <= c.failFirstN {
		return nil, c.failErr
	}
	return &core.CompletionResponse{Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: "done"}}}}, nil
}
func (c *reqCapturingClient) CreateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (c *reqCapturingClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "scripted", Context: 100000}
}
func (c *reqCapturingClient) Ping(context.Context) error { return nil }
func (c *reqCapturingClient) Close() error               { return nil }

// mockErrorHandler proves the errMgr wiring itself survived the migration —
// orchestrator.ErrorManager's actual thought_signature business logic can't
// be exercised from this package (kernel/agents can't import
// kernel/orchestrator without cycling), so this stands in for "some
// ErrorHandler said retry with modified history" and records that it was
// consulted with the real error.
type mockErrorHandler struct {
	sawErr  error
	handled bool
}

func (m *mockErrorHandler) Handle(err error, history []core.Message) (bool, []core.Message) {
	m.sawErr = err
	if m.handled {
		return false, history // only offer the fix once, like a real INVALID_ARGUMENT sanitizer would converge
	}
	m.handled = true
	// Mimic ErrorManager's actual repair: strip tool-call structure from the
	// last message, forcing modified=true so the loop retries.
	newHistory := append([]core.Message(nil), history...)
	return true, newHistory
}

func TestSubAgentRequestCarriesPurposeExecuteCeiling(t *testing.T) {
	c := &reqCapturingClient{}
	reg := tools.NewRegistry()
	a := spawnAgent(t, c, reg, core.SubAgentConfig{Role: core.RoleWorker, Goal: "say hi", MaxTurns: 3})

	if _, err := a.Execute(context.Background()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(c.got) == 0 {
		t.Fatal("client never received a request")
	}
	_, wantMaxTok, wantTemp := modelport.PolicyFor(modelport.PurposeExecute)
	req := c.got[0]
	if req.MaxTokens == nil || *req.MaxTokens != wantMaxTok {
		t.Errorf("MaxTokens = %v, want %d (this call sent no ceiling at all before)", req.MaxTokens, wantMaxTok)
	}
	if req.Temperature == nil || *req.Temperature != wantTemp {
		t.Errorf("Temperature = %v, want %f", req.Temperature, wantTemp)
	}
}

func TestSubAgentErrMgrRetryStillFiresThroughCompleteWith(t *testing.T) {
	invalidArgErr := &fakeAPIErr{msg: "API error 400: INVALID_ARGUMENT"}
	c := &reqCapturingClient{failFirstN: 1, failErr: invalidArgErr}
	reg := tools.NewRegistry()
	rtr := newScriptedRouter(c)
	eh := &mockErrorHandler{}
	f := NewAgentFactory(rtr, reg, nil, eh)
	a, err := f.Spawn(context.Background(), core.SubAgentConfig{Role: core.RoleWorker, Goal: "say hi", MaxTurns: 3})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	result, err := a.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v (errMgr-driven retry should have recovered)", err)
	}
	if !result.Success {
		t.Fatalf("expected success after the errMgr-driven retry, got: %+v", result)
	}
	if eh.sawErr == nil || eh.sawErr.Error() != invalidArgErr.Error() {
		t.Errorf("errMgr.Handle was not consulted with the real completion error, saw: %v", eh.sawErr)
	}
	if c.calls != 2 {
		t.Errorf("expected exactly 2 attempts (fail once, then succeed), got %d", c.calls)
	}
}

type fakeAPIErr struct{ msg string }

func (e *fakeAPIErr) Error() string { return e.msg }
