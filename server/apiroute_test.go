package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkcode/config"
)

// An unrouted /api/ path must fail as an API, not succeed as a web page.
//
// The SPA fallback answers unknown paths with index.html so client-side routes
// do not 404. Applied to /api/ that is actively harmful: a caller asking for a
// misspelled or removed endpoint got 200 text/html, a status claiming success,
// and a body that only fails later inside JSON.parse — pointing nowhere near
// the actual mistake. This is the same masking that once let missing font
// files pass a status-code assertion.
func TestUnroutedAPIPathsReturnJSON404(t *testing.T) {
	h := newTestServer(&config.Config{}).Handler()

	for _, path := range []string{
		"/api/skills",   // removed alias
		"/api/episodes", // removed alias
		"/api/nonsense", // never existed
		"/api/memory/typo",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))

			if w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (got %q)", w.Code, w.Header().Get("Content-Type"))
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON — an API caller cannot use HTML", ct)
			}
			var body map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Errorf("body is not JSON: %v", err)
			} else if body["error"] == nil {
				t.Error("a 404 should say what was not found")
			}
		})
	}
}

// The fallback must keep working for everything that is not an API path,
// otherwise client-side routing breaks.
func TestNonAPIPathsStillReachTheSPA(t *testing.T) {
	h := newTestServer(&config.Config{}).Handler()
	for _, path := range []string{"/", "/some/client/route"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 — client routes must not 404", path, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s served %q, want the app shell", path, w.Header().Get("Content-Type"))
		}
	}
}
