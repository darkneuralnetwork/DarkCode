package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDebugPathsDoNotGetTheAppShell. pprof is gated behind --debug, but the SPA
// fallback answered /debug/pprof/ with index.html and a 200, so probing whether
// the profiler is enabled returned "yes" either way. A disabled endpoint has to
// say it is absent.
func TestDebugPathsDoNotGetTheAppShell(t *testing.T) {
	h := webHandler()
	for _, p := range []string{"/debug/pprof/", "/debug/pprof/goroutine", "/debug/anything"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (body starts %q)", p, rec.Code,
				strings.TrimSpace(rec.Body.String())[:min(60, len(strings.TrimSpace(rec.Body.String())))])
		}
	}
}

// TestUnknownAppPathStillGetsTheShell — client-side routing must keep working.
func TestUnknownAppPathStillGetsTheShell(t *testing.T) {
	h := webHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/spa/route", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("SPA route = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Error("SPA route did not get the app shell")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
