package llm

import (
	"errors"
	"testing"

	"github.com/darkcode/core"
)

// APIError must be errors.Is-recognizable as ErrContextTooLong when the
// provider body signals an overflow, so callers can recover instead of
// treating it as fatal.
func TestAPIError_IsContextTooLong(t *testing.T) {
	overflow := []string{
		`{"error":{"code":"context_length_exceeded","message":"maximum context length is 8192 tokens"}}`,
		"the request exceeds the context window of this model",
		"llama_decode failed: n_ctx too small for prompt",
		"prompt is too long: 9000 tokens > 8192",
	}
	for _, body := range overflow {
		err := &APIError{Code: 400, Body: body}
		if !errors.Is(err, core.ErrContextTooLong) {
			t.Errorf("body %q should match ErrContextTooLong", body)
		}
	}

	notOverflow := []string{
		`{"error":{"code":"rate_limit_exceeded"}}`,
		"invalid api key",
		"internal server error",
	}
	for _, body := range notOverflow {
		err := &APIError{Code: 429, Body: body}
		if errors.Is(err, core.ErrContextTooLong) {
			t.Errorf("body %q should NOT match ErrContextTooLong", body)
		}
	}
}
