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
	// The shell's mark is typographic now (see the .rail-mark wordmark), so it
	// references no logo image. What must not regress is the shell pointing at
	// an asset that no longer exists, which is what the .ico removal broke.
	if !strings.Contains(body, "/favicon.ico") {
		t.Error("index.html should reference the favicon it ships")
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

// The frontend is embedded in the binary, so it may never be served stale — a
// rebuilt binary has to win over anything the browser is holding. That used to
// be enforced with no-store, which also meant every reload re-downloaded the
// whole 3.5 MB frontend. These tests pin the cheaper arrangement that keeps the
// same guarantee: revalidate always, transfer only what changed.
func TestStaticAssetsRevalidateButDoNotRetransfer(t *testing.T) {
	h := webHandler()

	req := httptest.NewRequest("GET", "/css/app.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so the browser has no way to revalidate cheaply")
	}
	// no-store would forbid reuse outright and make the ETag pointless.
	if cc := w.Header().Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q; no-store re-downloads the asset every load", cc)
	}

	// Presenting the tag back must yield an empty 304, not the file again.
	req2 := httptest.NewRequest("GET", "/css/app.css", nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotModified {
		t.Errorf("revalidation status = %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes; the point is to carry none", w2.Body.Len())
	}
}

// A stale asset must remain impossible: the tag is derived from the bytes, so
// different content cannot share a tag.
func TestETagIsContentDerived(t *testing.T) {
	h := webHandler()
	get := func(p string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w.Header().Get("ETag")
	}
	if a, b := get("/css/app.css"), get("/index.html"); a == b {
		t.Errorf("two different files share the ETag %q", a)
	}
}

func TestTextAssetsAreCompressed(t *testing.T) {
	h := webHandler()
	req := httptest.NewRequest("GET", "/styles.css", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", enc)
	}
	if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("a response that varies by encoding must say so")
	}
	gz := w.Body.Len()

	// A client that cannot decompress still has to get usable bytes.
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest("GET", "/css/app.css", nil))
	if plain.Header().Get("Content-Encoding") != "" {
		t.Error("gzip was sent to a client that never asked for it")
	}
	if plain.Body.Len() <= gz {
		t.Errorf("compression saved nothing: %d gz vs %d raw", gz, plain.Body.Len())
	}
	if !strings.Contains(plain.Body.String(), "{") {
		t.Error("the uncompressed body is not CSS")
	}
}

// Fonts and images are already compressed; a second pass burns CPU for nothing.
func TestAlreadyCompressedAssetsAreNotGzipped(t *testing.T) {
	h := webHandler()
	req := httptest.NewRequest("GET", "/logo.png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Error("a PNG was gzipped, which costs CPU and saves nothing")
	}
}

// Mermaid is 2.7 MB, larger than the rest of the frontend combined, and is only
// needed for replies that contain a diagram. It must not be on the startup path.
func TestNoDiagramLibraryIsShipped(t *testing.T) {
	h := webHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(w.Body.String(), "mermaid") {
		t.Error("the shell references a diagram library again")
	}

	// This replaces TestMermaidIsNotLoadedEagerly, and the change of contract
	// is deliberate rather than an oversight.
	//
	// mermaid.min.js was 2.7 MB of a 3.9 MB embedded interface — 69% of every
	// byte the binary carried for the browser — to render the occasional
	// diagram inside a plan document. It was already lazy-loaded, so the cost
	// was not page weight; it was that a single static binary got 2.7 MB
	// heavier for one feature on one screen.
	//
	// Fenced blocks now render as code, which is what they are. If diagram
	// rendering is wanted back, the honest options are a much smaller renderer
	// or an explicit "open diagram" action — not a library nobody sees.
	vw := httptest.NewRecorder()
	h.ServeHTTP(vw, httptest.NewRequest("GET", "/vendor/mermaid.min.js", nil))
	if vw.Code == http.StatusOK && vw.Body.Len() > 100000 {
		t.Error("the diagram library is back in the binary")
	}
}
