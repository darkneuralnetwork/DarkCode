package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkcode/core"
)

// TestBlankModelDefaultsToClientModel is the regression guard for the Gemini
// "model is not specified" blank-name error: a request built without a Model
// field must go out carrying the client's configured model, not "".
func TestBlankModelDefaultsToClientModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		gotModel = payload.Model
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key", "gemini-2.5-flash")

	// Non-streaming: blank Model must be filled from the client.
	if _, err := c.ChatCompletion(context.Background(), &core.CompletionRequest{
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotModel != "gemini-2.5-flash" {
		t.Errorf("non-stream sent model=%q, want the client's model", gotModel)
	}

	// An explicit model must still win.
	if _, err := c.ChatCompletion(context.Background(), &core.CompletionRequest{
		Model:    "explicit-model",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatCompletion explicit: %v", err)
	}
	if gotModel != "explicit-model" {
		t.Errorf("explicit model overridden: got %q", gotModel)
	}
}
