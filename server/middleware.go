package server

// middleware.go — the request-level policy every /api call passes through.
//
// Four concerns that are easy to reason about apart and confusing together:
// browser security headers, CORS, per-address rate limiting, and the
// cross-origin (CSRF) check. They were interleaved with routing, project
// summarisation and GUI session handling in one 1,100-line file, which is how
// a security check ends up read by nobody.

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// securityHeaders adds defense-in-depth browser security headers (nosniff,
// frame-deny, no-referrer) to every response. Cheap even though the server is
// loopback-only, in case the UI is ever proxied or framed.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isLocalhostOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a small, dependency-free per-remote-addr token bucket. It
// protects /api/* from a rogue or buggy client flooding the server (and
// exhausting LLM-provider budgets) since there is no other request
// throttling anywhere in front of the kernel.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
}

func newRateLimiter(ratePerSecond, burst float64) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    ratePerSecond,
		burst:   burst,
	}
}

// allow reports whether a request from key may proceed, consuming one token
// if so. Stale buckets are not actively swept; the map stays bounded in
// practice by the small number of distinct client addresses a loopback-only
// server ever sees.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	now := time.Now()
	if !ok {
		b = &tokenBucket{tokens: rl.burst - 1, lastFill: now}
		rl.buckets[key] = b
		return true
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimitMiddleware throttles /api/* requests per remote address. health
// checks and SSE are exempt (SSE holds one long-lived connection, not a
// stream of requests to throttle).
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/health" && r.URL.Path != "/api/events" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !s.apiRateLimiter.allow(host) {
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalhostOrigin reports whether an Origin header points at a loopback host.
func isLocalhostOrigin(o string) bool {
	for _, p := range []string{"http://127.0.0.1:", "http://localhost:", "https://127.0.0.1:", "https://localhost:", "http://[::1]:", "https://[::1]:"} {
		if strings.HasPrefix(o, p) {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether a Host header names this machine.
//
// The port is not checked — the listener already decides that — but the
// hostname must be one the browser could only have reached by resolving to
// loopback legitimately. A bare IPv6 host arrives bracketed; SplitHostPort
// unwraps it, and its failure means there was no port at all, so the whole
// value is the host.
func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// csrfMiddleware blocks drive-by cross-origin requests. The server is always
// bound to 127.0.0.1, so there is no remote attacker and no bearer token is
// needed — but a malicious website (evil.com) open in the user's browser can
// still issue fetch() calls to localhost. Any /api/* request (except
// /api/health) carrying a non-loopback Origin header is rejected.
//
// # ORIGIN IS NOT ENOUGH, AND THIS IS WHY THE HOST CHECK IS HERE
//
// Origin alone leaves DNS rebinding wide open, which SECURITY.md lists as in
// scope for this server. The attack does not send a foreign Origin — it
// removes the need for one:
//
//	evil.com resolves to the attacker, serves a page, and re-resolves to
//	127.0.0.1 on a short TTL. The page then fetches http://evil.com:12345/api/…
//	The browser considers that SAME-ORIGIN with the page it is running, so it
//	sends no Origin header at all, `origin != ""` is false, and the request
//	arrives at a handler that will read and write files.
//
// The request still has to carry `Host: evil.com`, because that is the name
// the browser dialled. Requiring the Host to be loopback is what closes it, and
// it is the standard defence precisely because the attacker cannot forge it
// without giving up the rebind.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every path, not just /api/: the UI itself is worth protecting, and a
		// rebound page loading it is the first step to driving it.
		if r.Host != "" && !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "blocked: this server only answers to a loopback host name")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/health" {
			if origin := r.Header.Get("Origin"); origin != "" && !isLocalhostOrigin(origin) {
				writeError(w, http.StatusForbidden, "blocked: cross-origin requests are not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
