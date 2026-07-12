package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// webFS embeds the frontend UI assets (index.html, styles.css, app.js)
// into the binary at compile time. Zero external dependencies.
//
//go:embed web/*
var webFS embed.FS

// webHandler returns an http.Handler that serves the embedded web UI.
// The UI is a single-page application; unknown paths fall back to index.html
// so client-side navigation never 404s.
//
// The server is always loopback (127.0.0.1) and same-origin with the SPA, so
// no auth token or CORS headers are injected here. Drive-by cross-origin
// requests are blocked by csrfMiddleware on /api/*.
func webHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("server: embedded web assets missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The embedded assets are baked into the binary at compile time, so a
		// rebuilt binary always carries a fresh frontend. We disable browser
		// caching entirely (no-store) so a reload after a rebuild always picks
		// up the new app.js/styles.css instead of serving a stale copy.
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		// http.FileServer expects paths with a leading slash and strips it
		// internally before looking up the file. fs.Stat, however, uses
		// paths relative to the sub-FS root (no leading slash), so we must
		// normalise before checking whether the file actually exists.
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			clean = "."
		}
		// SPA fallback: if the requested path is not a real embedded file,
		// serve index.html instead of a 404.
		if info, statErr := fs.Stat(sub, clean); statErr != nil || info.IsDir() {
			if statErr != nil || clean != "." {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}
