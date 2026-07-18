package llm

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/darkcode/core"
)

// countingClient records how many times ChatCompletion is invoked and always
// fails with a fixed error — so a test can assert whether the retry wrapper
// treated that error as transient (multiple attempts) or permanent (one).
type countingClient struct {
	calls int
	err   error
}

func (c *countingClient) ChatCompletion(ctx context.Context, req *core.CompletionRequest) (*core.CompletionResponse, error) {
	c.calls++
	return nil, c.err
}
func (c *countingClient) ChatCompletionStream(ctx context.Context, req *core.CompletionRequest, cb *core.StreamCallbacks) (*core.CompletionResponse, error) {
	c.calls++
	return nil, c.err
}
func (c *countingClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, c.err
}
func (c *countingClient) ModelInfo() core.ModelMetadata { return core.ModelMetadata{ID: "counting"} }
func (c *countingClient) Ping(ctx context.Context) error { return nil }
func (c *countingClient) Close() error                   { return nil }

// TestRetryable_UnsupportedSchemeNotRetried is the regression guard for the
// "who is pm of india" retry storm: a request with an empty/invalid BaseURL
// fails with *url.Error "unsupported protocol scheme", which also satisfies
// net.Error — it must NOT be retried five times.
func TestRetryable_UnsupportedSchemeNotRetried(t *testing.T) {
	urlErr := &url.Error{Op: "Post", URL: "/chat/completions", Err: errors.New(`unsupported protocol scheme ""`)}
	inner := &countingClient{err: urlErr}
	rc := WithRetry(inner, DefaultRetryOpts) // MaxAttempts = 5

	if _, err := rc.ChatCompletion(context.Background(), &core.CompletionRequest{}); err == nil {
		t.Fatal("expected an error")
	}
	if inner.calls != 1 {
		t.Fatalf("unsupported-scheme error must not be retried: got %d attempts, want 1", inner.calls)
	}
}

// TestRetryable_NoBaseURLNotRetried asserts the client-level ErrNoBaseURL
// guard is classified permanent (single attempt).
func TestRetryable_NoBaseURLNotRetried(t *testing.T) {
	inner := &countingClient{err: ErrNoBaseURL}
	rc := WithRetry(inner, DefaultRetryOpts)

	if _, err := rc.ChatCompletion(context.Background(), &core.CompletionRequest{}); !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("expected ErrNoBaseURL, got %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("ErrNoBaseURL must not be retried: got %d attempts, want 1", inner.calls)
	}
}

// TestRetryable_TimeoutStillRetried guards against over-broadening: a genuine
// network timeout (Timeout()==true) must remain retryable.
func TestRetryable_TimeoutStillRetried(t *testing.T) {
	timeoutErr := &url.Error{Op: "Post", URL: "https://api.example.com", Err: timeoutError{}}
	inner := &countingClient{err: timeoutErr}
	rc := WithRetry(inner, RetryOpts{MaxAttempts: 3})

	if _, err := rc.ChatCompletion(context.Background(), &core.CompletionRequest{}); err == nil {
		t.Fatal("expected an error")
	}
	if inner.calls != 3 {
		t.Fatalf("a genuine timeout should still be retried: got %d attempts, want 3", inner.calls)
	}
}

// timeoutError is a net.Error that reports Timeout()==true.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestClient_EmptyBaseURLFailsFast asserts a bare *Client with no BaseURL
// never attempts an HTTP call and returns ErrNoBaseURL.
func TestClient_EmptyBaseURLFailsFast(t *testing.T) {
	c := NewClient("", "", "some-model")
	if _, err := c.ChatCompletion(context.Background(), &core.CompletionRequest{}); !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("empty BaseURL should return ErrNoBaseURL, got %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("Ping with empty BaseURL should return ErrNoBaseURL, got %v", err)
	}
	if _, err := c.CreateEmbedding(context.Background(), "text"); !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("CreateEmbedding with empty BaseURL should return ErrNoBaseURL, got %v", err)
	}
}
