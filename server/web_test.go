package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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
	css := fw.Body.String()
	if strings.Contains(css, "gstatic.com") {
		t.Error("fonts.css still references gstatic.com — woff2 URLs were not localized")
	}

	// Every font the CSS names must actually be served.
	//
	// Checking only that fonts.css exists left a hole big enough to walk
	// through: the stylesheet was committed while the .woff2 files it points
	// at were not, so a fresh clone answered 404 for all thirteen and the
	// browser quietly fell back to a system font. Nothing failed, the test
	// stayed green, and the offline guarantee was gone.
	faces := regexp.MustCompile(`url\(([^)]+\.woff2)\)`).FindAllStringSubmatch(css, -1)
	if len(faces) == 0 {
		t.Fatal("fonts.css names no woff2 files — the vendoring step did not run")
	}
	// Checking the status code is not enough. webHandler falls back to
	// index.html for any path it cannot find, so a missing font answers 200
	// with a page of HTML — the browser gets markup where it expected a
	// typeface, fails quietly, and substitutes a system font. The only honest
	// test is whether the bytes are a font.
	seen := map[string]bool{}
	for _, m := range faces {
		ref := strings.Trim(m[1], `"'`)
		if seen[ref] {
			continue
		}
		seen[ref] = true

		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", ref, nil))
		body := w.Body.Bytes()
		if w.Code != http.StatusOK {
			t.Errorf("font %s: status %d", ref, w.Code)
			continue
		}
		// woff2 files begin with the signature "wOF2".
		if len(body) < 4 || string(body[:4]) != "wOF2" {
			t.Errorf("font %s: served %d bytes that are not woff2 — referenced by "+
				"fonts.css but not embedded, so the SPA fallback returned index.html",
				ref, len(body))
		}
	}
}
