package core

import (
	"context"
	"errors"
	"testing"
)

// fakeLoRAClient is a minimal LLMClient that also implements LoRAManager,
// recording mount scale changes so the test can assert mount→run→unmount.
type fakeLoRAClient struct {
	mounts   []string // "name=scale" in call order
	mountErr error
}

func (f *fakeLoRAClient) ChatCompletion(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	return &CompletionResponse{}, nil
}
func (f *fakeLoRAClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error) {
	return &CompletionResponse{}, nil
}
func (f *fakeLoRAClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (f *fakeLoRAClient) ModelInfo() ModelMetadata { return ModelMetadata{ID: "fake"} }
func (f *fakeLoRAClient) Ping(ctx context.Context) error { return nil }
func (f *fakeLoRAClient) Close() error                    { return nil }
func (f *fakeLoRAClient) MountLoRA(name string, scale float32) error {
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mounts = append(f.mounts, name+"="+formatScale(scale))
	return nil
}

func formatScale(s float32) string {
	if s == 1.0 {
		return "1"
	}
	return "0"
}

// plainClient is an LLMClient with NO LoRA support — WithLoRA must run fn on
// the base model without touching adapters.
type plainClient struct{ fakeLoRAClient }

func (plainClient) MountLoRA(name string, scale float32) error { panic("must not be called") }

func TestWithLoRA_MountsRunsUnmounts(t *testing.T) {
	c := &fakeLoRAClient{}
	ran := false
	err := WithLoRA(c, "coding", nil, func() error { ran = true; return nil })
	if err != nil {
		t.Fatalf("WithLoRA err: %v", err)
	}
	if !ran {
		t.Error("fn was not run")
	}
	if len(c.mounts) != 2 || c.mounts[0] != "coding=1" || c.mounts[1] != "coding=0" {
		t.Errorf("expected mount coding=1 then coding=0, got %v", c.mounts)
	}
}

func TestWithLoRA_UnknownTaskRunsBaseModel(t *testing.T) {
	c := &fakeLoRAClient{}
	err := WithLoRA(c, "no-such-task", nil, func() error { return nil })
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(c.mounts) != 0 {
		t.Errorf("unknown task must not mount anything, got %v", c.mounts)
	}
}

func TestWithLoRA_MountFailureFallsThroughAndLogs(t *testing.T) {
	c := &fakeLoRAClient{mountErr: errors.New("adapter missing")}
	logged := false
	ran := false
	err := WithLoRA(c, "coding", func(string, ...interface{}) { logged = true }, func() error { ran = true; return nil })
	if err != nil {
		t.Fatalf("mount failure must not be fatal, got: %v", err)
	}
	if !ran {
		t.Error("fn must still run on the base model when mount fails")
	}
	if !logged {
		t.Error("mount failure must be logged, not swallowed")
	}
}

func TestWithLoRA_PropagatesFnError(t *testing.T) {
	c := &fakeLoRAClient{}
	want := errors.New("boom")
	got := WithLoRA(c, "summarize", nil, func() error { return want })
	if !errors.Is(got, want) {
		t.Errorf("WithLoRA must propagate fn's error, got %v", got)
	}
	// Still unmounted despite the error.
	if len(c.mounts) != 2 || c.mounts[1] != "summarizer=0" {
		t.Errorf("adapter must be unmounted even on fn error, got %v", c.mounts)
	}
}

func TestTaskLoRARegistry(t *testing.T) {
	for task, want := range map[string]string{
		"local_compression": "summarizer",
		"coding":            "coding",
		"planning":          "planner",
	} {
		if TaskLoRA[task] != want {
			t.Errorf("TaskLoRA[%q] = %q, want %q", task, TaskLoRA[task], want)
		}
	}
}

var _ = context.Background
