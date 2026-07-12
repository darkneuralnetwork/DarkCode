package scheduler

// priority_queue.go — priority-ordered task queue with context-cancellable Pop.
//
// Previously Pop() used sync.Cond.Wait() which could not be interrupted by
// context cancellation (the code even acknowledged: "In a real system, you'd
// use select over a channel"). This is now a channel-based implementation:
// Push sends to a buffered channel after inserting in priority order, and Pop
// selects on either the task channel or ctx.Done().

import (
	"context"
	"errors"
	"sync"
)

// TaskPriority orders tasks by importance.
type TaskPriority int

const (
	PriorityBackground TaskPriority = iota // Background indexing, garbage collection
	PriorityLow                            // Pre-fetching
	PriorityNormal                         // User-queued tasks
	PriorityHigh                           // Interactive user queries
)

// Task represents a unit of work that needs to be scheduled.
type Task struct {
	ID       string
	Priority TaskPriority
	Execute  func(ctx context.Context) error
}

// PriorityQueue manages tasks based on priority.
type PriorityQueue struct {
	mu     sync.Mutex
	tasks  []*Task
	notify chan struct{} // signal that a task is available
}

func NewPriorityQueue() *PriorityQueue {
	return &PriorityQueue{
		notify: make(chan struct{}, 1),
	}
}

// Push adds a task to the queue (priority-ordered) and wakes up a worker.
func (pq *PriorityQueue) Push(task *Task) error {
	if task == nil {
		return errors.New("cannot push nil task")
	}

	pq.mu.Lock()
	// Insert in priority order (higher priority first).
	inserted := false
	for i, t := range pq.tasks {
		if task.Priority > t.Priority {
			pq.tasks = append(pq.tasks[:i], append([]*Task{task}, pq.tasks[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted {
		pq.tasks = append(pq.tasks, task)
	}
	pq.mu.Unlock()

	// Wake up one waiting worker (non-blocking; if nobody is waiting, the
	// signal is buffered and the next Pop will pick it up).
	select {
	case pq.notify <- struct{}{}:
	default:
	}
	return nil
}

// Pop blocks until a task is available or ctx is cancelled.
func (pq *PriorityQueue) Pop(ctx context.Context) (*Task, error) {
	for {
		pq.mu.Lock()
		if len(pq.tasks) > 0 {
			task := pq.tasks[0]
			pq.tasks = pq.tasks[1:]
			pq.mu.Unlock()
			return task, nil
		}
		pq.mu.Unlock()

		// Wait for a push or cancellation.
		select {
		case <-pq.notify:
			// loop back and try to pop
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Len returns the current queue depth.
func (pq *PriorityQueue) Len() int {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	return len(pq.tasks)
}
