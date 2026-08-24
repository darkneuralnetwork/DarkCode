package modelport

// purpose_context_test.go — confirms Manager.Complete tags the outgoing
// context with the Ask's Purpose (llm.WithPurpose), and that the tag
// actually reaches model/llm's call log — so a request can be attributed to
// the subsystem that made it ("execute", "plan", "compress", ...) instead of
// just a bare model name. Verified end-to-end through the real retry-wrapped
// client, not by inspecting an unexported context key from another package.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/model/llm"
)

type purposeTestClient struct{}

func (purposeTestClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	return &core.CompletionResponse{Choices: []core.ChatChoice{{
		Message: core.ResponseMessage{Role: "assistant", Content: "ok"},
	}}}, nil
}
func (c purposeTestClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return c.ChatCompletion(ctx, req)
}
func (purposeTestClient) CreateEmbedding(context.Context, string) ([]float32, error) { return nil, nil }
func (purposeTestClient) ModelInfo() core.ModelMetadata {
	return core.ModelMetadata{ID: "purpose-model", Context: 8000}
}
func (purposeTestClient) Ping(context.Context) error { return nil }
func (purposeTestClient) Close() error               { return nil }

type purposeTestRouter struct{ client core.LLMClient }

func (r *purposeTestRouter) Route(tier core.ModelTier, complexity int, desc string) (core.LLMClient, string, error) {
	return r.client, "purpose-model", nil
}

func TestCompletePurposeReachesTheCallLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm_calls.jsonl")
	llm.SetCallLogPath(path)
	t.Cleanup(func() { llm.SetCallLogPath("") })

	retrying := llm.WithRetry(purposeTestClient{}, llm.RetryOpts{MaxAttempts: 1})
	m, err := New(&purposeTestRouter{client: retrying})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Complete(context.Background(), Ask{Purpose: PurposeCompress, Messages: msgs}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open call log: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	found := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e llm.CallLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		if e.Purpose == string(PurposeCompress) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the call log to record purpose=\"compress\", found no matching entry")
	}
}
