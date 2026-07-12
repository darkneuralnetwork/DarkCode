package memory

import (
	"fmt"
	"sync"
	"testing"

	"github.com/darkcode/core"
)

// TestSystemConcurrentAccess exercises every major write/read pair on System
// from many goroutines at once. Run with `go test -race` — the audit found
// this package had zero concurrency coverage, so `-race` passing was not
// meaningful evidence of thread-safety, only evidence that nothing had ever
// exercised it. This test doesn't assert on values (interleaving makes exact
// outcomes nondeterministic by design) — its job is to give the race
// detector real concurrent traffic to catch data races in.
func TestSystemConcurrentAccess(t *testing.T) {
	sys := newTestSystem(t)

	const goroutines = 20
	const opsPerGoroutine = 25

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := fmt.Sprintf("g%d-%d", g, i)

				sys.STMAdd(core.Message{Role: core.RoleUser, Content: id})
				_ = sys.STMGet()

				_ = sys.EpisodicAdd(core.EpisodicEntry{TaskGoal: id, Outcome: "success", Summary: id})
				_ = sys.EpisodicGet()
				_ = sys.EpisodicSearch(id)

				_ = sys.SemanticAdd(id, "content for "+id, "test", []string{"concurrency"})
				_, _ = sys.SemanticGet(id)
				_ = sys.SemanticAll()

				_ = sys.ProceduralAdd(&core.Skill{Name: id, Description: "test skill"})
				_, _ = sys.ProceduralGet(id)

				sys.ArchitectureAddDecision(id, "decision for "+id)
				_ = sys.ArchitectureDecisions()

				_ = sys.Summary()
			}
		}(g)
	}
	wg.Wait()

	// Sanity: writes actually landed (not a correctness contract on
	// interleaving, just confirming the concurrent adds weren't silently lost
	// wholesale, which would suggest a lock isn't doing its job).
	if got := len(sys.EpisodicGet()); got != goroutines*opsPerGoroutine {
		t.Errorf("EpisodicGet() returned %d entries, want %d", got, goroutines*opsPerGoroutine)
	}
	if got := len(sys.SemanticAll()); got != goroutines*opsPerGoroutine {
		t.Errorf("SemanticAll() returned %d entries, want %d", got, goroutines*opsPerGoroutine)
	}
}

// TestSystemConcurrentSTMCompress specifically races STMAdd against
// STMCompress/STMSetMax, since STMCompress mutates the whole slice (not an
// append) and is the STM operation most likely to race with a concurrent
// append if the locking were ever loosened.
func TestSystemConcurrentSTMCompress(t *testing.T) {
	sys := newTestSystem(t)
	for i := 0; i < 30; i++ {
		sys.STMAdd(core.Message{Role: core.RoleUser, Content: fmt.Sprintf("seed-%d", i)})
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			sys.STMAdd(core.Message{Role: core.RoleAssistant, Content: fmt.Sprintf("added-%d", i)})
		}
	}()
	go func() {
		defer wg.Done()
		briefing := []core.Message{{Role: core.RoleSystem, Content: "compressed briefing"}}
		for i := 0; i < 10; i++ {
			sys.STMCompress(briefing, 4)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			sys.STMSetMax(20 + i)
			_ = sys.STMGet()
		}
	}()
	wg.Wait()
}
