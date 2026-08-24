package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler records how many times it actually ran and returns a body
// that reflects the run count, so tests can tell a real execution from a replay.
func countingHandler(runs *int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(runs, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"run":` + strconv.Itoa(int(n)) + `}`))
	})
}

func newIdemServer() *Server {
	return &Server{idempotency: newIdempotencyStore(time.Minute)}
}

func do(h http.Handler, method, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/chat", nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func TestIdempotencyNoKeyPassesThrough(t *testing.T) {
	var runs int32
	h := newIdemServer().idempotencyMiddleware(countingHandler(&runs))
	do(h, http.MethodPost, "")
	do(h, http.MethodPost, "")
	if runs != 2 {
		t.Fatalf("no key: expected both requests to run, got %d runs", runs)
	}
}

func TestIdempotencyReplaysCompleted(t *testing.T) {
	var runs int32
	h := newIdemServer().idempotencyMiddleware(countingHandler(&runs))

	first := do(h, http.MethodPost, "abc")
	second := do(h, http.MethodPost, "abc")

	if runs != 1 {
		t.Fatalf("same key: handler should run once, ran %d times", runs)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay body mismatch: %q vs %q", first.Body.String(), second.Body.String())
	}
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", second.Code)
	}
	if second.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("replay lost Content-Type header")
	}
}

func TestIdempotencyDifferentKeysIndependent(t *testing.T) {
	var runs int32
	h := newIdemServer().idempotencyMiddleware(countingHandler(&runs))
	do(h, http.MethodPost, "k1")
	do(h, http.MethodPost, "k2")
	if runs != 2 {
		t.Fatalf("distinct keys should each run, got %d", runs)
	}
}

func TestIdempotencyInFlightReturns409(t *testing.T) {
	s := newIdemServer()
	// Simulate an in-flight request by registering the key without completing.
	s.idempotency.begin("busy")

	h := s.idempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run while an identical request is in flight")
	}))
	rw := do(h, http.MethodPost, "busy")
	if rw.Code != http.StatusConflict {
		t.Fatalf("in-flight duplicate: status = %d, want 409", rw.Code)
	}
}

func TestIdempotencyAbortAllowsRetry(t *testing.T) {
	s := newIdemServer()
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := s.idempotencyMiddleware(panicky)

	func() {
		defer func() { _ = recover() }()
		do(h, http.MethodPost, "kaboom")
	}()

	// The aborted key must be retryable, not wedged as "in progress".
	found, _, _, _, _ := s.idempotency.begin("kaboom")
	if found {
		t.Fatal("panicked attempt should have been aborted, leaving the key free")
	}
}

func TestIdempotencyTTLEviction(t *testing.T) {
	s := &Server{idempotency: newIdempotencyStore(10 * time.Millisecond)}
	var runs int32
	h := s.idempotencyMiddleware(countingHandler(&runs))
	do(h, http.MethodPost, "temp")
	time.Sleep(20 * time.Millisecond)
	do(h, http.MethodPost, "temp") // key expired -> runs again
	if runs != 2 {
		t.Fatalf("expired key should re-run, got %d runs", runs)
	}
}
