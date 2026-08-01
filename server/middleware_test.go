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
