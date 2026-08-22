package server

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/darkcode/surfaces/ui"
)

// newProgressTestServer builds the minimum Server needed to exercise the
// deadline: an emitter with the heartbeat handler attached.
//
// The emitter is left in its default (non-SSE) mode on purpose. Subscriber
// fan-out is gated on the output mode, so a heartbeat built on Subscribe()
// would silently deliver nothing here — and, more to the point, in any
// deployment whose mode was changed. The handler path this asserts is
// unconditional.
func newProgressTestServer() *Server {
	s := &Server{emitter: ui.NewEventEmitter(false, nil)}
	s.watchProgress()
	return s
}

// TestProgressExtendsTheDeadline is the point of the file: a turn that keeps
// reporting progress must outlive the idle window. Under the flat five-minute
// cap this replaces, a build that was working steadily got cancelled mid-step
// simply for having taken longer than the budget.
//
// idle/tick margin: this used to be idle=150ms with a 50ms tick — only a 3x
// margin against the wall clock, which flaked under load (go test ./... runs
// many packages in parallel, and -race adds real per-goroutine/per-channel
// overhead) even though progress_deadline.go's timer-reset logic is
// standard, correct Go with no race in it — a delayed tick delivery under
// scheduler/GC pressure, not a production bug, was enough to blow a 150ms
// budget. Widened to a 10x margin (500ms idle / 50ms tick), which is what
// actually makes this load-independent rather than just less likely to
// flake.
func TestProgressExtendsTheDeadline(t *testing.T) {
	s := newProgressTestServer()
	const idle = 500 * time.Millisecond

	ctx, cancel := s.progressContext(context.Background(), idle, 10*time.Second)
	defer cancel()

	// Report progress every 50ms for well past the idle window.
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for done := false; !done; {
		select {
		case <-tick.C:
			s.emitter.EmitTaskUpdate("worker", "running", "still going")
		case <-ctx.Done():
			t.Fatal("cancelled while progress was still being reported")
		case <-deadline:
			done = true
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("context ended early: %v", ctx.Err())
	}
}

// TestSilenceCancels is the other half. A deadline that only ever extends is
// not a deadline; a run that has gone quiet must still be cut off.
func TestSilenceCancels(t *testing.T) {
	s := newProgressTestServer()
	ctx, cancel := s.progressContext(context.Background(), 100*time.Millisecond, 10*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a silent run was never cancelled")
	}
}

// TestHardCapBoundsEvenBusyRuns guards the failure mode the idle window
// introduces: an agent emitting events forever would otherwise never stop.
func TestHardCapBoundsEvenBusyRuns(t *testing.T) {
	s := newProgressTestServer()
	ctx, cancel := s.progressContext(context.Background(), time.Second, 200*time.Millisecond)
	defer cancel()

	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.emitter.EmitTaskUpdate("worker", "running", "busy")
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the hard cap did not bound a continuously-busy run")
	}
}

// TestCancelIsIdempotent — the CancelFunc is deferred and may also be called
// explicitly; a second close() would panic the request goroutine.
func TestCancelIsIdempotent(t *testing.T) {
	s := newProgressTestServer()
	_, cancel := s.progressContext(context.Background(), time.Second, time.Second)
	cancel()
	cancel()
}

// TestHeartbeatStopsAfterCancel: once a turn is over its beat channel must be
// detached, so a later event can't be delivered into a finished turn's slot.
func TestHeartbeatStopsAfterCancel(t *testing.T) {
	s := newProgressTestServer()
	_, cancel := s.progressContext(context.Background(), time.Second, time.Second)
	cancel()

	s.emitter.EmitTaskUpdate("worker", "running", "late event")

	s.progressMu.Lock()
	ch := s.progressCh
	s.progressMu.Unlock()
	if ch != nil {
		t.Error("progress channel still attached after the turn was cancelled")
	}
}

// TestWatchdogGoroutinesDoNotLeak. One watchdog is started per chat turn, so
// anything that lets it outlive its request accumulates for the life of the
// process — and the GUI is long-running. Sampling pprof on a live server is
// misleading here (goroutines caught mid-exit read as leaks), so this asserts
// it directly.
func TestWatchdogGoroutinesDoNotLeak(t *testing.T) {
	cases := []struct {
		name string
		run  func(s *Server)
	}{
		{"cancelled immediately", func(s *Server) {
			for i := 0; i < 50; i++ {
				_, cancel := s.progressContext(context.Background(), time.Minute, time.Hour)
				cancel()
			}
		}},
		{"cancelled after a beat", func(s *Server) {
			for i := 0; i < 50; i++ {
				_, cancel := s.progressContext(context.Background(), time.Minute, time.Hour)
				s.emitter.EmitTaskUpdate("w", "running", "beat")
				cancel()
			}
		}},
		{"overlapping turns", func(s *Server) {
			var wg sync.WaitGroup
			for i := 0; i < 50; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, cancel := s.progressContext(context.Background(), time.Minute, time.Hour)
					s.emitter.EmitTaskUpdate("w", "running", "beat")
					cancel()
				}()
			}
			wg.Wait()
		}},
		{"cancelled twice concurrently", func(s *Server) {
			for i := 0; i < 50; i++ {
				_, cancel := s.progressContext(context.Background(), time.Minute, time.Hour)
				var wg sync.WaitGroup
				for j := 0; j < 2; j++ {
					wg.Add(1)
					go func() { defer wg.Done(); cancel() }()
				}
				wg.Wait()
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newProgressTestServer()
			base := runtime.NumGoroutine()
			tc.run(s)
			// Exit is asynchronous, so settle rather than sampling once.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if runtime.NumGoroutine() <= base+2 {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatalf("goroutines base=%d now=%d — watchdogs outlived their turns",
				base, runtime.NumGoroutine())
		})
	}
}
