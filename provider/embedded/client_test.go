package embedded

import (
	"testing"

	"github.com/darkcode/llm"
)

// TestNewEmbeddedClientUsesLocalTimeout. A local model on constrained
// hardware routinely needs far more than a cloud provider's timeout for a
// single generation — this used to inherit llm.DefaultTimeout (300s) with no
// override, and a real run against a small CPU-bound model exceeded it and
// failed outright with no retry possible. The embedded client must get the
// longer, local-appropriate ceiling.
func TestNewEmbeddedClientUsesLocalTimeout(t *testing.T) {
	c := NewEmbeddedClient("http://127.0.0.1:0/v1", "embedded", "test-model")
	if c.HTTPClient == nil {
		t.Fatal("HTTPClient is nil")
	}
	if got := c.HTTPClient.Timeout; got != llm.LocalTimeout {
		t.Errorf("HTTPClient.Timeout = %v, want llm.LocalTimeout (%v)", got, llm.LocalTimeout)
	}
}
