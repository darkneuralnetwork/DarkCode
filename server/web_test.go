package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebHandlerServesLogoPNG guards the logo fix: the header <img> now points
// at /logo.png (a real PNG) instead of /logo.ico (an .ico that browsers render
// unreliably in <img>, which showed as a broken image). This verifies the
// embedded asset is actually served with a 200 and non-empty body.
func TestWebHandlerServesLogoPNG(t *testing.T) {
	h := webHandler()
	req := httptest.NewRequest("GET", "/logo.png", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /logo.png status = %d, want 200", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatal("GET /logo.png returned an empty body")
	}
	// http.FileServer sniffs content type; a PNG must not be served as text.
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "image/") {
		t.Errorf("Content-Type = %q, want an image/* type", ct)
	}
}

// TestWebHandlerNoLongerReferencesLogoIco verifies the index page references
// the PNG (the .ico was removed), so the broken-image regression can't return.
func TestWebHandlerServesIndexWithPNGLogo(t *testing.T) {
	h := webHandler()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/logo.png") {
		t.Error("index.html should reference /logo.png for the header logo")
	}
	if strings.Contains(body, "/logo.ico") {
		t.Error("index.html still references the removed /logo.ico")
	}
}

// TestWebHandlerServesVendoredAssetsOffline verifies the CDN libraries and
// fonts are vendored + embedded (so the GUI works air-gapped), and that the
// index page no longer pulls chart.js/mermaid/Google Fonts from the network.
func TestWebHandlerServesVendoredAssetsOffline(t *testing.T) {
	h := webHandler()
	for _, path := range []string{"/vendor/chart.umd.min.js", "/vendor/mermaid.min.js", "/fonts/fonts.css"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("GET %s: status=%d len=%d, want 200 with a non-empty body (asset not embedded?)", path, w.Code, w.Body.Len())
		}
	}

	// The index must not reference external CDNs / Google Fonts at runtime.
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, ext := range []string{"cdn.jsdelivr.net", "fonts.googleapis.com", "fonts.gstatic.com"} {
		if strings.Contains(body, ext) {
			t.Errorf("index.html still references external host %q — offline goal broken", ext)
		}
	}

	// The vendored font CSS must not point back at gstatic.
	fr := httptest.NewRequest("GET", "/fonts/fonts.css", nil)
	fw := httptest.NewRecorder()
	h.ServeHTTP(fw, fr)
	if strings.Contains(fw.Body.String(), "gstatic.com") {
		t.Error("fonts.css still references gstatic.com — woff2 URLs were not localized")
	}
}
