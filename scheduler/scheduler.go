package scheduler

// scheduler.go — Scheduler manages CPU, memory, and context budgeting for
// background tasks (indexing, pre-fetching, model loading). It is the
// "Model Manager" backbone (spec §4): it coordinates the WorkerPool,
// PriorityQueue, ModelLoader, and MemoryBudget.
//
// Previously Run()/Stop() were empty stubs and the scheduler didn't wire up
// its own subsystems. It now:
//   - Starts a WorkerPool on Run()
//   - Accepts tasks via Submit()
//   - Tracks active tasks and shuts them down cleanly on Stop()
//   - Exposes the ModelLoader for model load/unload with memory budgeting

import (
	"context"
	"errors"
	"sync"
)

// Scheduler manages CPU, memory, and context budgeting for tasks.
type Scheduler struct {
	maxConcurrency int
	queue          *PriorityQueue
	workers        *WorkerPool
	loader         *ModelLoader
	memory         *MemoryBudget
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.Mutex
	running        bool
}

// NewScheduler creates a scheduler with the given max concurrency.
func NewScheduler(maxConcurrency int) *Scheduler {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	mem := NewMemoryBudget(defaultMemoryBudget)
	return &Scheduler{
		maxConcurrency: maxConcurrency,
		queue:          NewPriorityQueue(),
		loader:         NewModelLoader(mem),
		memory:         mem,
	}
}

// defaultMemoryBudget is a conservative default (8 GB). In production this
// would be detected from the system.
const defaultMemoryBudget = 8 * 1024 * 1024 * 1024

// Run starts the scheduler loop and worker pool in the background.
func (s *Scheduler) Run() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.workers = NewWorkerPool(s.queue, s.maxConcurrency)
	s.workers.Start(s.ctx)
	s.running = true
	s.mu.Unlock()
}

// Stop halts the scheduler and cancels all active tasks.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.cancel()
	s.running = false
	s.mu.Unlock()
	s.wg.Wait()
}

// Submit enqueues a task for background execution.
func (s *Scheduler) Submit(task *Task) error {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if !running {
		return errors.New("scheduler is not running; call Run() first")
	}
	return s.queue.Push(task)
}

// BudgetContext checks if adding tokenCount exceeds the budget.
func (s *Scheduler) BudgetContext(tokenCount int, maxBudget int) error {
	if tokenCount > maxBudget {
		return errors.New("context budget exceeded")
	}
	return nil
}

// ModelLoader exposes the model loader for model management.
func (s *Scheduler) ModelLoader() *ModelLoader { return s.loader }

// MemoryBudget exposes the memory budget for allocation queries.
func (s *Scheduler) MemoryBudget() *MemoryBudget { return s.memory }

// QueueDepth returns the number of pending tasks.
func (s *Scheduler) QueueDepth() int { return s.queue.Len() }

// IsRunning reports whether the scheduler is active.
func (s *Scheduler) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
