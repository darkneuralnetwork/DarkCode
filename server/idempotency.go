package server

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Idempotency for /api/chat. A chat turn runs the agent — writing files,
// creating projects, spending tokens — so a retried POST (double-submit, a
// client/proxy retry after a slow response) must not re-execute it. Clients
// opt in by sending an Idempotency-Key header; the same key replays the first
// response instead of running the work again. Requests without a key are
// unaffected.

type idemState int

const (
	idemInflight idemState = iota
	idemDone
)

type idemEntry struct {
	state  idemState
	status int
	header http.Header
	body   []byte
	at     time.Time
}

type idempotencyStore struct {
	mu  sync.Mutex
	m   map[string]*idemEntry
	ttl time.Duration
}

func newIdempotencyStore(ttl time.Duration) *idempotencyStore {
	return &idempotencyStore{m: make(map[string]*idemEntry), ttl: ttl}
}

// begin registers key as in-flight, or reports the existing entry. When found
// is true the caller must NOT run the work: done distinguishes a completed
// request (replay status/header/body) from one still in flight (return 409).
func (s *idempotencyStore) begin(key string) (found, done bool, status int, header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	if e, ok := s.m[key]; ok {
		if e.state == idemDone {
			return true, true, e.status, e.header, e.body
		}
		return true, false, 0, nil, nil
	}
	s.m[key] = &idemEntry{state: idemInflight, at: time.Now()}
	return false, false, 0, nil, nil
}

// complete records the terminal response for key so later retries replay it.
func (s *idempotencyStore) complete(key string, status int, header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = &idemEntry{state: idemDone, status: status, header: header, body: body, at: time.Now()}
}

// abort drops an in-flight entry so an attempt that produced no response (e.g.
// a handler panic) can be retried rather than being wedged as "in progress".
func (s *idempotencyStore) abort(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[key]; ok && e.state == idemInflight {
		delete(s.m, key)
	}
}

func (s *idempotencyStore) evictLocked() {
	for k, e := range s.m {
		if time.Since(e.at) > s.ttl {
			delete(s.m, k)
		}
	}
}

// responseRecorder buffers a handler's response so it can be both cached and
// replayed to the real client. /api/chat returns a single JSON body (streaming
// is on the separate SSE endpoint), so full buffering is fine.
type responseRecorder struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.body.Write(b)
}

func writeRecorded(w http.ResponseWriter, status int, header http.Header, body []byte) {
	for k, v := range header {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// idempotencyMiddleware de-duplicates POSTs that carry an Idempotency-Key. No
// key (or non-POST) passes straight through, preserving existing behavior.
func (s *Server) idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		found, done, status, header, body := s.idempotency.begin(key)
		if found {
			if done {
				writeRecorded(w, status, header, body)
				return
			}
			writeError(w, http.StatusConflict, "a request with this Idempotency-Key is already being processed")
			return
		}

		rec := newResponseRecorder()
		completed := false
		defer func() {
			if !completed {
				s.idempotency.abort(key) // handler panicked; let a retry through
			}
		}()
		next.ServeHTTP(rec, r)
		completed = true

		s.idempotency.complete(key, rec.status, rec.header, rec.body.Bytes())
		writeRecorded(w, rec.status, rec.header, rec.body.Bytes())
	})
}
