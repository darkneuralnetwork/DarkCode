package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/darkcode/config"
)

// The server binds to loopback, so there is no remote attacker — but a page
// the user has open in their browser can still fetch() localhost. The Origin
// check is the entire defence, and it had no tests.

func TestIsLocalhostOriginAcceptsLoopback(t *testing.T) {
	for _, o := range []string{
		"http://127.0.0.1:12345",
		"https://127.0.0.1:12345",
		"http://localhost:12345",
		"https://localhost:3000",
		"http://[::1]:12345",
		"https://[::1]:8080",
	} {
		if !isLocalhostOrigin(o) {
			t.Errorf("%q was rejected; the UI's own origin must be allowed", o)
		}
	}
}

// TestIsLocalhostOriginRejectsLookalikes. Every one of these begins with
// something loopback-shaped. A prefix match that stopped before the port
// separator would accept them all.
func TestIsLocalhostOriginRejectsLookalikes(t *testing.T) {
	for _, o := range []string{
		"http://evil.com",
		"https://evil.com",
		"http://127.0.0.1.evil.com:80",
		"http://localhost.evil.com:80",
		"https://localhost-evil.com:443",
		"http://notlocalhost:12345",
		"http://127.0.0.1evil:12345",
		"file://",
		"null",
		"",
	} {
		if isLocalhostOrigin(o) {
			t.Errorf("%q was accepted as loopback", o)
		}
	}
}

// TestCSRFBlocksCrossOrigin drives the middleware rather than the predicate,
// since the middleware is what an attacker actually reaches.
func TestCSRFBlocksCrossOrigin(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	reached := false
	h := s.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/api/chat", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if reached {
		t.Error("a cross-origin request reached the handler")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestCSRFAllowsTheUIAndDirectClients(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})

	cases := map[string]string{
		"the UI's own origin": "http://localhost:12345",
		"no Origin at all":    "", // curl, the CLI, any non-browser client
	}
	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			reached := false
			h := s.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
			}))
			req := httptest.NewRequest("POST", "/api/chat", nil)
			// httptest defaults Host to example.com, which the rebinding
			// check now refuses. A real client always dials loopback.
			req.Host = "localhost:12345"
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if !reached {
				t.Error("a legitimate request was blocked")
			}
		})
	}
}

// TestCSRFExemptsHealthOnly. The health endpoint is deliberately open so a
// probe works from anywhere; nothing else may be.
func TestCSRFExemptsHealthOnly(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})

	for path, wantReached := range map[string]bool{
		"/api/health": true,
		"/api/chat":   false,
		"/api/config": false,
		"/api/events": false,
	} {
		reached := false
		h := s.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
		}))
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "localhost:12345"
		req.Header.Set("Origin", "https://evil.com")
		h.ServeHTTP(httptest.NewRecorder(), req)

		if reached != wantReached {
			t.Errorf("%s: reached=%v, want %v", path, reached, wantReached)
		}
	}
}

// TestRateLimiterSpendsThenRefills. Without a cap, one rogue client can drain
// the provider budget as fast as the network allows.
func TestRateLimiterSpendsThenRefills(t *testing.T) {
	rl := newRateLimiter(100, 3) // 3 burst, refills fast

	for i := 0; i < 3; i++ {
		if !rl.allow("client") {
			t.Fatalf("request %d denied inside the burst of 3", i+1)
		}
	}
	if rl.allow("client") {
		t.Error("a fourth request passed with the burst spent")
	}

	time.Sleep(50 * time.Millisecond) // 100/s refills the bucket
	if !rl.allow("client") {
		t.Error("the bucket never refilled")
	}
}

// TestRateLimiterIsPerClient. One noisy caller must not lock out another.
func TestRateLimiterIsPerClient(t *testing.T) {
	rl := newRateLimiter(0.0001, 2)

	for rl.allow("noisy") {
		// drain
	}
	if !rl.allow("quiet") {
		t.Error("a second client was denied because the first exhausted its own bucket")
	}
}

// TestRateLimiterExemptsHealthAndEvents. A blocked health check reads as the
// server being down, and SSE is one long connection rather than a flood.
func TestRateLimiterExemptsHealthAndEvents(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	s.apiRateLimiter = newRateLimiter(0.0001, 1) // effectively exhausted after one

	call := func(path string) int {
		h := s.rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:5555"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	// Spend the single token on a throttled path.
	call("/api/config")
	if got := call("/api/config"); got != http.StatusTooManyRequests {
		t.Errorf("a throttled path returned %d, want 429", got)
	}
	for _, exempt := range []string{"/api/health", "/api/events"} {
		if got := call(exempt); got == http.StatusTooManyRequests {
			t.Errorf("%s was rate limited", exempt)
		}
	}
}

// TestCSRFBlocksDNSRebinding covers the hole the Origin check could never see.
//
// A rebinding attacker does not send a foreign Origin — they remove the need
// for one. evil.com serves a page, re-resolves to 127.0.0.1 on a short TTL,
// and the page fetches http://evil.com:12345/api/…. The browser treats that as
// SAME-ORIGIN with the page, so it sends no Origin header at all and the
// `origin != ""` guard never fires. What the request must still carry is
// Host: evil.com, because that is the name the browser dialled.
//
// Against the Origin-only middleware every case below reaches the handler.
func TestCSRFBlocksDNSRebinding(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})

	blocked := map[string]string{
		"rebound host, no Origin at all": "evil.com:12345",
		"rebound host without a port":    "evil.com",
		"a LAN address is not loopback":  "192.168.1.10:12345",
		"public IP dialled directly":     "203.0.113.9:12345",
		"loopback-looking but is not":    "127.0.0.1.evil.com:12345",
	}
	for name, host := range blocked {
		t.Run(name, func(t *testing.T) {
			reached := false
			h := s.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
			}))
			// Deliberately no Origin header: that is the whole point.
			req := httptest.NewRequest("POST", "/api/workspace/file", nil)
			req.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if reached {
				t.Errorf("a request for Host %q reached the handler", host)
			}
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
		})
	}

	// The real UI must keep working, including the loopback forms a browser
	// and a direct client actually send.
	for name, host := range map[string]string{
		"the UI's own host": "localhost:12345",
		"dotted loopback":   "127.0.0.1:12345",
		"another loopback":  "127.0.0.53:12345",
		"IPv6 loopback":     "[::1]:12345",
		"no port":           "localhost",
	} {
		t.Run(name, func(t *testing.T) {
			reached := false
			h := s.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest("GET", "/api/status", nil)
			req.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if !reached {
				t.Errorf("Host %q was blocked; status %d", host, w.Code)
			}
		})
	}
}

// loopbackRequest builds a request the way a real client reaches this server.
//
// httptest.NewRequest defaults Host to example.com, which csrfMiddleware now
// refuses as a DNS-rebinding attempt. Every test that drives the full handler
// stack needs a Host the server would actually be dialled on, and saying so
// once beats a literal in each fixture.
func loopbackRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "localhost:12345"
	return r
}
