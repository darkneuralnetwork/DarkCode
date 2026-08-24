package observability

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Guard is what keeps a panicking background task from taking the process
// down, and six call sites now depend on it — including the file watcher,
// which parses whatever turns up in the workspace. It had no tests, so
// nothing checked that the thing doing the protecting actually works.

func TestGuardSwallowsAPanic(t *testing.T) {
	Guard("test", func() { panic("boom") }) // must return normally
}

func TestGuardRunsTheFunctionAndReturnsAfterIt(t *testing.T) {
	ran := false
	Guard("test", func() { ran = true })
	if !ran {
		t.Error("Guard did not run the function")
	}
}

// TestGuardHandlesEveryPanicValue. A panic carries whatever was passed to it;
// formatting only strings would itself panic on the others.
func TestGuardHandlesEveryPanicValue(t *testing.T) {
	values := []any{
		"a string",
		errors.New("an error"),
		42,
		struct{ X int }{1},
		[]string{"a", "b"},
		nil, // panic(nil) is a real thing
	}
	for _, v := range values {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Guard let a panic(%v) escape: %v", v, r)
				}
			}()
			Guard("test", func() { panic(v) })
		}()
	}
}

// TestGuardIsNotOneShot. A watcher calls its callback on every tick; a guard
// that only survived the first panic would still lose the process on the
// second file.
func TestGuardIsNotOneShot(t *testing.T) {
	for i := 0; i < 5; i++ {
		Guard("test", func() { panic("again") })
	}
}

// TestGuardDoesNotSuppressNormalReturn. Recovering must not swallow the work
// itself — a guarded task still has to do its job.
func TestGuardDoesNotSuppressNormalReturn(t *testing.T) {
	calls := 0
	for i := 0; i < 3; i++ {
		Guard("test", func() { calls++ })
	}
	if calls != 3 {
		t.Errorf("guarded function ran %d times, want 3", calls)
	}
}

// TestGoRunsOnItsOwnGoroutineUnderGuard is the property the callers rely on:
// the panic happens somewhere else and the caller keeps going.
func TestGoRunsOnItsOwnGoroutineUnderGuard(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	Go("test", func() {
		defer wg.Done()
		panic("on another goroutine")
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the guarded goroutine never ran")
	}
	// Reaching here at all is the assertion: an unguarded panic on a goroutine
	// terminates the whole test binary rather than failing this test.
}

// TestGoDoesNotBlockTheCaller. Callers start background work and continue; a
// synchronous Go would stall the request that launched it.
func TestGoDoesNotBlockTheCaller(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	Go("test", func() {
		close(started)
		<-release
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the goroutine never started")
	}
	close(release) // the caller got here while the task was still running
}

// TestConcurrentGuardsAreIndependent. Several background tasks run at once;
// one panicking must not disturb the others.
func TestConcurrentGuardsAreIndependent(t *testing.T) {
	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	completed := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		i := i
		Go("test", func() {
			defer wg.Done()
			if i%2 == 0 {
				panic("even tasks panic")
			}
			mu.Lock()
			completed++
			mu.Unlock()
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guarded goroutines did not all finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if completed != n/2 {
		t.Errorf("%d of the %d non-panicking tasks completed", completed, n/2)
	}
}
