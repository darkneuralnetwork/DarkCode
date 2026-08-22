package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkcode/config"
)

func postModelsPing(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/models/ping", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleModelsPing(w, req)
	return w
}

func TestHandleModelsPingRejectsNonPOST(t *testing.T) {
	s := newTestServer(&config.Config{})
	req := httptest.NewRequest("GET", "/api/models/ping", nil)
	w := httptest.NewRecorder()
	s.handleModelsPing(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleModelsPingRejectsInvalidJSON(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := postModelsPing(s, `{not valid`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleModelsPingRejectsUnknownProvider(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := postModelsPing(s, `{"provider":"not-a-real-provider"}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleModelsPingBlocksSSRF verifies the same SSRF guard used by
// handleModelsFetch also protects handleModelsPing — both accept a
// caller-supplied base_url that triggers an outbound request, so both need
// the same deny-by-default posture for non-local destinations.
func TestHandleModelsPingBlocksSSRF(t *testing.T) {
	s := newTestServer(&config.Config{})
	// "openai-compatible" is a CustomBaseURL provider, so a caller-supplied
	// base_url is honoured — pointed at a private/loopback address it must
	// be blocked rather than silently probed.
	w := postModelsPing(s, `{"provider":"openai-compatible","base_url":"http://169.254.169.254/latest/meta-data"}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (SSRF-blocked destination)", w.Code, http.StatusBadRequest)
	}
}

func postModelsDisable(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/models/disable", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleModelsDisable(w, req)
	return w
}

func postModelsEnable(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/models/enable", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleModelsEnable(w, req)
	return w
}

func TestHandleModelsDisableRejectsNonPOST(t *testing.T) {
	s := newTestServer(&config.Config{})
	req := httptest.NewRequest("GET", "/api/models/disable", nil)
	w := httptest.NewRecorder()
	s.handleModelsDisable(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleModelsDisableRejectsMissingModel(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := postModelsDisable(s, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleModelsDisableRejectsInvalidDuration(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := postModelsDisable(s, `{"model":"gpt-4o","duration":"not-a-duration"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleModelsDisableReportsUnavailableWithNoKernel(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := postModelsDisable(s, `{"model":"gpt-4o"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleModelsEnableRejectsNonPOST(t *testing.T) {
	s := newTestServer(&config.Config{})
	req := httptest.NewRequest("GET", "/api/models/enable", nil)
	w := httptest.NewRecorder()
	s.handleModelsEnable(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleModelsEnableRejectsMissingModel(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := postModelsEnable(s, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
