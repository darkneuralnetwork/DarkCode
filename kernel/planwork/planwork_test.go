package planwork

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

type fakeClient struct {
	calls int
	reply string
	err   error
}

func (f *fakeClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &core.CompletionResponse{
		Choices: []core.ChatChoice{{Message: core.ResponseMessage{Role: "assistant", Content: f.reply}}},
	}, nil
}

func (f *fakeClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	return f.ChatCompletion(ctx, req)
}
func (f *fakeClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (f *fakeClient) ModelInfo() core.ModelMetadata  { return core.ModelMetadata{} }
func (f *fakeClient) Ping(ctx context.Context) error { return nil }
func (f *fakeClient) Close() error                   { return nil }

// TestAmendCostsOneCall is the reason this package exists. The console's copy
// spent two calls per turn — one per document — while the web's spent one.
func TestAmendCostsOneCall(t *testing.T) {
	fc := &fakeClient{reply: "# Plan\nnew plan\n===WORKFLOW===\n- [ ] T1: do it"}

	plan, workflow := Amend(context.Background(), fc, "m", "add auth", "# Plan\nold", "- [ ] T1: old")

	if fc.calls != 1 {
		t.Errorf("Amend made %d LLM calls, want exactly 1", fc.calls)
	}
	if !strings.Contains(plan, "new plan") {
		t.Errorf("plan not updated: %q", plan)
	}
	if !strings.Contains(workflow, "T1: do it") {
		t.Errorf("workflow not updated: %q", workflow)
	}
}

// TestAmendNeverLosesDocumentsOnFailure — a plan refresh runs after work that
// already succeeded. Returning an empty plan because a model call failed would
// destroy the user's document to report a transient error.
func TestAmendNeverLosesDocumentsOnFailure(t *testing.T) {
	oldPlan, oldWorkflow := "# Plan\nkeep me", "- [ ] T1: keep me"

	for name, fc := range map[string]*fakeClient{
		"error":         {err: context.DeadlineExceeded},
		"empty reply":   {reply: ""},
		"garbage reply": {reply: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			plan, workflow := Amend(context.Background(), fc, "m", "do something", oldPlan, oldWorkflow)
			if plan != oldPlan || workflow != oldWorkflow {
				t.Errorf("documents changed on failure: plan=%q workflow=%q", plan, workflow)
			}
		})
	}
}

func TestAmendWithNilClientIsANoOp(t *testing.T) {
	plan, workflow := Amend(context.Background(), nil, "m", "q", "P", "W")
	if plan != "P" || workflow != "W" {
		t.Errorf("nil client changed the documents: %q / %q", plan, workflow)
	}
}

func TestSplit(t *testing.T) {
	cases := []struct{ in, wantPlan, wantWF string }{
		{"A\n===WORKFLOW===\nB", "A", "B"},
		{"A\n=== WORKFLOW ===\nB", "A", "B"},
		{"# Plan\ntext\n\n## Tasks\n- [ ] T1: x", "# Plan\ntext", "## Tasks\n- [ ] T1: x"},
		{"only a plan", "only a plan", ""},
	}
	for _, c := range cases {
		p, w := Split(c.in)
		if p != c.wantPlan || w != c.wantWF {
			t.Errorf("Split(%q) = (%q, %q), want (%q, %q)", c.in, p, w, c.wantPlan, c.wantWF)
		}
	}
}

func TestTaskStatuses(t *testing.T) {
	got := TaskStatuses("- [ ] T1: a\n- [/] T2: b\n- [x] T3: c\n- [X] T4: d\nnot a task")
	want := map[string]string{"T1": "pending", "T2": "running", "T3": "done", "T4": "done"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestInjectNodeStatusIsDeterministic — the class lines are generated from a
// map, so without the sort the same plan would re-render in a different order
// each time and show as a spurious diff on every turn.
func TestInjectNodeStatusIsDeterministic(t *testing.T) {
	plan := "# Plan\n```mermaid\ngraph TD\nT1-->T2\n```"
	workflow := "- [x] T1: a\n- [/] T2: b\n- [ ] T3: c"

	first := InjectNodeStatus(plan, workflow)
	for i := 0; i < 20; i++ {
		if got := InjectNodeStatus(plan, workflow); got != first {
			t.Fatalf("InjectNodeStatus is not deterministic:\n%q\nvs\n%q", got, first)
		}
	}
	for _, want := range []string{"class T1 done", "class T2 running", "class T3 pending"} {
		if !strings.Contains(first, want) {
			t.Errorf("missing %q in:\n%s", want, first)
		}
	}
}

func TestInjectNodeStatusNoMermaidIsNoOp(t *testing.T) {
	plan := "# Plan\nno graph here"
	if got := InjectNodeStatus(plan, "- [x] T1: a"); got != plan {
		t.Errorf("plan changed without a mermaid fence: %q", got)
	}
}
