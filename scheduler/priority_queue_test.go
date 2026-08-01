package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The queue decides what the workers do next. A priority inversion here means
// an interactive query waits behind background indexing, which reads as the
// tool being slow rather than as a scheduling bug.

func task(id string, p TaskPriority) *Task {
	return &Task{ID: id, Priority: p}
}

func TestHigherPriorityPopsFirstRegardlessOfPushOrder(t *testing.T) {
	pq := NewPriorityQueue()

	// Pushed worst-first, so ordering can only come from the priority.
	for _, tk := range []*Task{
		task("background", PriorityBackground),
		task("low", PriorityLow),
		task("normal", PriorityNormal),
		task("high", PriorityHigh),
	} {
		if err := pq.Push(tk); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	want := []string{"high", "normal", "low", "background"}
	for _, id := range want {
		got, err := pq.Pop(context.Background())
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if got.ID != id {
			t.Fatalf("popped %q, want %q", got.ID, id)
		}
	}
}

// TestEqualPrioritiesKeepArrivalOrder. Without FIFO within a level, two
// user queries can complete out of order for no visible reason.
func TestEqualPrioritiesKeepArrivalOrder(t *testing.T) {
	pq := NewPriorityQueue()
	for _, id := range []string{"first", "second", "third"} {
		if err := pq.Push(task(id, PriorityNormal)); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []string{"first", "second", "third"} {
		got, err := pq.Pop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want {
			t.Errorf("popped %q, want %q — arrival order was not kept", got.ID, want)
		}
	}
}

// TestLatecomerWithHigherPriorityJumpsTheQueue. This is the whole point of a
// priority queue: an interactive query arriving during a backlog goes first.
func TestLatecomerWithHigherPriorityJumpsTheQueue(t *testing.T) {
	pq := NewPriorityQueue()
	for i := 0; i < 5; i++ {
		if err := pq.Push(task("bg", PriorityBackground)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pq.Push(task("interactive", PriorityHigh)); err != nil {
		t.Fatal(err)
	}

	got, err := pq.Pop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "interactive" {
		t.Errorf("popped %q, want the interactive task to jump ahead", got.ID)
	}
}

func TestPushRejectsNil(t *testing.T) {
	pq := NewPriorityQueue()
	if err := pq.Push(nil); err == nil {
		t.Error("pushing nil succeeded; a worker would dereference it")
	}
	if pq.Len() != 0 {
		t.Errorf("a rejected push still changed the depth to %d", pq.Len())
	}
}

// TestPopBlocksUntilSomethingArrives. Workers call Pop in a loop; returning
// immediately on an empty queue would spin a core.
func TestPopBlocksUntilSomethingArrives(t *testing.T) {
	pq := NewPriorityQueue()
	popped := make(chan *Task, 1)

	go func() {
		tk, err := pq.Pop(context.Background())
		if err == nil {
			popped <- tk
		}
	}()

	select {
	case tk := <-popped:
		t.Fatalf("Pop returned %v from an empty queue", tk)
	case <-time.After(100 * time.Millisecond):
		// still blocked, which is correct
	}

	if err := pq.Push(task("late", PriorityNormal)); err != nil {
		t.Fatal(err)
	}
	select {
	case tk := <-popped:
		if tk.ID != "late" {
			t.Errorf("popped %q", tk.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pop did not wake when a task arrived")
	}
}

// TestPopReturnsOnCancellation. Shutdown cancels the context; a Pop that
// ignored it would keep the process alive forever.
func TestPopReturnsOnCancellation(t *testing.T) {
	pq := NewPriorityQueue()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := pq.Pop(ctx)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Pop returned no error after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pop ignored the cancelled context")
	}
}

// TestPopDrainsAnAlreadyCancelledContextQueue. A cancelled context must not
// stop a worker collecting work that is already queued — losing a task is
// worse than one late shutdown.
func TestPopReturnsQueuedWorkEvenWhenCancelled(t *testing.T) {
	pq := NewPriorityQueue()
	if err := pq.Push(task("queued", PriorityNormal)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tk, err := pq.Pop(ctx)
	if err != nil || tk == nil {
		t.Fatalf("a queued task was dropped on a cancelled context: %v", err)
	}
	if tk.ID != "queued" {
		t.Errorf("popped %q", tk.ID)
	}
}

func TestLenTracksDepth(t *testing.T) {
	pq := NewPriorityQueue()
	if pq.Len() != 0 {
		t.Fatalf("a new queue reports depth %d", pq.Len())
	}
	for i := 0; i < 3; i++ {
		if err := pq.Push(task("t", PriorityNormal)); err != nil {
			t.Fatal(err)
		}
	}
	if pq.Len() != 3 {
		t.Errorf("depth = %d, want 3", pq.Len())
	}
	if _, err := pq.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pq.Len() != 2 {
		t.Errorf("depth after one pop = %d, want 2", pq.Len())
	}
}

// TestConcurrentPushAndPopLosesNothing. Run under -race, this is the check
// that the lock actually covers the slice surgery Push performs.
func TestConcurrentPushAndPopLosesNothing(t *testing.T) {
	pq := NewPriorityQueue()
	const producers, each = 8, 50
	total := producers * each

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		p := p
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_ = pq.Push(task("t", TaskPriority(p%4)))
			}
		}()
	}

	got := make(chan struct{}, total)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for c := 0; c < 4; c++ {
		go func() {
			for {
				if _, err := pq.Pop(ctx); err != nil {
					return
				}
				got <- struct{}{}
			}
		}()
	}

	wg.Wait()
	for i := 0; i < total; i++ {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d tasks came back out", i, total)
		}
	}
}
