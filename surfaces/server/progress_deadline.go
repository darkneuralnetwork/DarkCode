package server

// progress_deadline.go — a request deadline that measures silence, not work.
//
// A chat turn used to run under a flat five-minute timeout covering the kernel
// call and both completeness auto-continue passes. That is a budget on how long
// the task is ALLOWED to take, which is the wrong quantity: a build that is
// steadily writing files, running tests and reporting each step is healthy at
// six minutes, while a run that has emitted nothing for six minutes is stuck
// whether or not it started ten seconds ago. Under the flat cap the healthy
// build got cancelled mid-step — the reported "it closes automatically after
// some time" — and the stuck one was allowed to sit there for the full five.
//
// So the deadline resets on evidence of progress. The agent already narrates
// itself through the event emitter (every loop iteration, every tool call,
// every streamed chunk, every DAG node), so that stream is the heartbeat and
// nothing in the execution path needs to know this exists.
//
// A hard cap still applies, because "it is still emitting events" is not proof
// of usefulness — an agent can loop productively-looking forever.
//
// The heartbeat is taken from an emitter HANDLER rather than a Subscribe()
// channel, for two reasons that both matter. Subscriber fan-out is gated on the
// output mode (OutputSSE/OutputBoth), so a mode change elsewhere would silently
// stop the heartbeat and start cancelling every long turn at the idle mark,
// with nothing in the logs to say why. And subscriber sends are dropped when a
// channel is full, which makes a heartbeat that is least reliable exactly when
// the agent is busiest. Handlers are invoked unconditionally on every Emit.

import (
	"context"
	"sync"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/observability"
)

const (
	// chatIdleTimeout is how long a turn may go with NO reported progress
	// before it is treated as stuck. It has to comfortably exceed the longest
	// silent step the agent can take — a single large build or test run
	// reports nothing between "tool requested" and "tool completed" — so it is
	// deliberately much larger than a per-call timeout.
	chatIdleTimeout = 10 * time.Minute

	// chatHardTimeout bounds the whole turn regardless of progress. The loop's
	// own iteration and correction budgets are the real limit on runaway work;
	// this is the backstop for the case where those fail to bind.
	chatHardTimeout = 2 * time.Hour
)

// watchProgress registers the single, server-lifetime handler that feeds the
// request deadline. Called once from Start.
//
// One handler for the process, rather than one per request, is deliberate:
// EventEmitter.RemoveHandler matches by code pointer, so every per-request
// closure built at the same source line looks identical to it and de-registering
// one would de-register them all.
func (s *Server) watchProgress() {
	if s.emitter == nil {
		return
	}
	s.emitter.OnHandler(func(core.UIEvent) {
		s.progressMu.Lock()
		ch := s.progressCh
		s.progressMu.Unlock()
		if ch == nil {
			return
		}
		// Non-blocking: the watchdog only needs to know that SOMETHING
		// happened since it last looked, so a coalesced single-slot signal is
		// exactly right and can never block an emitting goroutine.
		select {
		case ch <- struct{}{}:
		default:
		}
	})
}

// progressContext returns a context cancelled after idle elapses with no
// emitted event, or after hard elapses in total, whichever comes first.
//
// The returned CancelFunc must be called to release the watchdog; it is safe to
// call more than once.
func (s *Server) progressContext(parent context.Context, idle, hard time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	beat := make(chan struct{}, 1)
	s.progressMu.Lock()
	s.progressCh = beat
	s.progressMu.Unlock()

	done := make(chan struct{})
	observability.Go("chat-progress-deadline", func() {
		idleTimer := time.NewTimer(idle)
		defer idleTimer.Stop()
		hardTimer := time.NewTimer(hard)
		defer hardTimer.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-hardTimer.C:
				observability.Log().Warn("chat turn hit its hard time limit and was cancelled",
					map[string]interface{}{"limit": hard.String()})
				cancel()
				return
			case <-idleTimer.C:
				// Say so. A turn that dies for lack of progress looks
				// identical to one that finished quietly, and guessing which
				// happened is what made the original flat timeout so hard to
				// diagnose.
				observability.Log().Warn("chat turn reported no progress and was cancelled as stuck",
					map[string]interface{}{"idle_limit": idle.String()})
				cancel()
				return
			case <-beat:
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(idle)
			}
		}
	})

	// sync.Once, not a plain bool: this CancelFunc is both deferred by the
	// request goroutine AND published as s.activeChatCancel for the stop
	// endpoint to call, so two goroutines really can reach it at once. An
	// unsynchronised flag would race, and losing that race means closing an
	// already-closed channel, which panics.
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(done)
			s.progressMu.Lock()
			// Only detach if this turn is still the active one — a turn that
			// finishes after a newer one started must not clear the newer
			// turn's heartbeat out from under it.
			if s.progressCh == beat {
				s.progressCh = nil
			}
			s.progressMu.Unlock()
		})
		cancel()
	}
}
