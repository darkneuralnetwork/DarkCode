package server

import "github.com/darkcode/safeurl"

// ssrfGuard validates that a URL is safe for the server to fetch on behalf of
// a user. Delegates to the shared safeurl package. Pass allowLoopback=true
// only for explicitly local destinations (e.g. an Ollama server).
//
// This prevents the server from being abused as an SSRF proxy to reach
// internal services or cloud metadata endpoints (e.g. 169.254.169.254).
func ssrfGuard(rawURL string, allowLoopback bool) bool {
	return safeurl.IsSafeFetchURL(rawURL, allowLoopback)
}
