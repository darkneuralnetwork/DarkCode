package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// webFS embeds the frontend UI assets (index.html, styles.css, app.js)
// into the binary at compile time. Zero external dependencies.
//
//go:embed web/*
var webFS embed.FS

// asset is one embedded file, prepared once at startup.
//
// Both representations are kept because the choice is per-request: a client
// that does not advertise gzip still has to be served something.
type asset struct {
	body        []byte
	gzipped     []byte // nil when compression does not pay for this file
	contentType string
	etag        string
}

var (
	assetsOnce sync.Once
	assets     map[string]*asset
)

// compressible reports whether a file is worth gzipping.
//
// Text compresses by 70–80%; the fonts, images and archives are already
// compressed internally, where a second pass costs CPU and returns nothing.
func compressible(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".css", ".html", ".json", ".svg", ".txt", ".map":
		return true
	}
	return false
}

// buildAssets walks the embedded filesystem once and precomputes what every
// response needs: the content type, a content hash for the ETag, and the gzip
// encoding.
//
// Doing this at startup rather than per request matters because the assets are
// immutable — they are compiled into the binary — so the work has exactly one
// correct answer and recomputing it for every reload is pure waste. Gzipping
// mermaid on demand would cost tens of milliseconds of CPU per page load.
func buildAssets() {
	assets = map[string]*asset{}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("server: embedded web assets missing: " + err.Error())
	}
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, rerr := fs.ReadFile(sub, p)
		if rerr != nil {
			return nil
		}
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = http.DetectContentType(body)
		}
		sum := sha256.Sum256(body)
		a := &asset{
			body:        body,
			contentType: ct,
			// A strong validator: the tag is the content, so a rebuilt binary
			// necessarily produces a different tag and the browser refetches.
			etag: `"` + hex.EncodeToString(sum[:12]) + `"`,
		}
		if compressible(p) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			if _, werr := zw.Write(body); werr == nil && zw.Close() == nil {
				// Only keep it if it actually helped; a compressed copy that is
				// bigger than the original is worse than not having one.
				if buf.Len() < len(body) {
					a.gzipped = append([]byte(nil), buf.Bytes()...)
				}
			}
		}
		assets[p] = a
		return nil
	})
}

// webHandler returns an http.Handler that serves the embedded web UI.
// The UI is a single-page application; unknown paths fall back to index.html
// so client-side navigation never 404s.
//
// The server is always loopback (127.0.0.1) and same-origin with the SPA, so
// no auth token or CORS headers are injected here. Drive-by cross-origin
// requests are blocked by csrfMiddleware on /api/*.
//
// Caching deserves a note, because the obvious safe choice is the expensive
// one. The assets are baked into the binary, so a rebuilt binary always carries
// a fresh frontend, and the handler used to send `no-store` to guarantee a
// reload could never serve a stale app.js. That guarantee is worth keeping and
// it was being bought at full price: every reload re-downloaded the entire
// frontend, 3.5 MB of it, because nothing was ever allowed to be reused.
//
// An ETag buys the same guarantee for nothing. `no-cache` still forces the
// browser to revalidate before using a cached copy — it may never serve one
// blind — but revalidation against an unchanged binary returns 304 with an
// empty body instead of the file. Stale content remains impossible, since the
// tag is a hash of the bytes: change the binary and every tag changes with it.
func webHandler() http.Handler {
	assetsOnce.Do(buildAssets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if clean == "" || clean == "." {
			clean = "index.html"
		}
		a, ok := assets[clean]
		if !ok {
			// SPA fallback: an unknown path is client-side navigation, not a
			// missing file, so the app shell answers it.
			a, ok = assets["index.html"]
			if !ok {
				http.NotFound(w, r)
				return
			}
		}

		h := w.Header()
		h.Set("Content-Type", a.contentType)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("ETag", a.etag)
		// Revalidate every time, but transfer only what changed.
		h.Set("Cache-Control", "no-cache")
		// The response body differs by encoding, so any cache between here and
		// the browser has to key on it.
		h.Set("Vary", "Accept-Encoding")

		// A matching tag means the browser already holds these exact bytes.
		if match := r.Header.Get("If-None-Match"); match != "" {
			for _, tag := range strings.Split(match, ",") {
				if strings.TrimSpace(tag) == a.etag || strings.TrimSpace(tag) == "*" {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}

		body := a.body
		if a.gzipped != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			body = a.gzipped
		}
		// ServeContent would renegotiate the type and ranges against the
		// uncompressed length, which is wrong once a gzip body is chosen.
		h.Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, bytes.NewReader(body))
	})
}
