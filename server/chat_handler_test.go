package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkcode/config"
)

// newTestServer builds a Server with only cfg populated. This is sufficient
// for handleChat's input-validation paths, which all return before touching
// the registry/memory/kernel — a full end-to-end handleChat test would need
// a mock LLM client and router, which is out of scope for a handler-level
// test (see orchestrator's own dispatch tests for kernel-level coverage).
func newTestServer(cfg *config.Config) *Server {
	return NewServer(cfg, nil, nil, nil, nil, nil, nil, nil)
}

func postChat(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleChat(w, req)
	return w
}

func TestHandleChatRejectsNonPOST(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	req := httptest.NewRequest("GET", "/api/chat", nil)
	w := httptest.NewRecorder()
	s.handleChat(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/chat: status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleChatRejectsInvalidJSON(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	w := postChat(s, `{not valid json`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleChatRejectsEmptyQuery(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	w := postChat(s, `{"query": ""}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "query is required") {
		t.Errorf("body = %q, want it to mention the missing query", w.Body.String())
	}
}

func TestHandleChatRejectsWhenNoModelConfigured(t *testing.T) {
	s := newTestServer(&config.Config{}) // no Model, no EnableLocalLLM
	w := postChat(s, `{"query": "hello"}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "select a model") {
		t.Errorf("body = %q, want it to mention selecting a model", w.Body.String())
	}
}

// TestHandleChatRejectsOversizedBody verifies the Phase-0 http.MaxBytesReader
// fix actually rejects a body over maxChatBodyBytes instead of buffering it
// unbounded.
func TestHandleChatRejectsOversizedBody(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	oversized := `{"query": "` + strings.Repeat("a", maxChatBodyBytes+1024) + `"}`

	w := postChat(s, oversized)

	if w.Code == http.StatusOK {
		t.Fatal("an oversized request body must be rejected, not processed")
	}
}

func TestHandleHTPRejectsOversizedBody(t *testing.T) {
	s := newTestServer(&config.Config{})
	oversized := `{"htp_version":"1.0","action":"health","extra":"` + strings.Repeat("a", maxHTPBodyBytes+1024) + `"}`

	req := httptest.NewRequest("POST", "/api/htp", bytes.NewBufferString(oversized))
	w := httptest.NewRecorder()
	s.handleHTP(w, req)

	// The HTP protocol always responds 200 and signals failure via the JSON
	// body's "ok" field, so the oversized-body rejection shows up there, not
	// as an HTTP status code.
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("an oversized HTP request body must be rejected (ok:false), got body: %s", w.Body.String())
	}
}
