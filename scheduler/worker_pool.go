package scheduler

import (
	"context"
	"log"
	"sync"
)

// WorkerPool manages a pool of workers that execute tasks from the PriorityQueue.
type WorkerPool struct {
	queue   *PriorityQueue
	workers int
	wg      sync.WaitGroup
}

func NewWorkerPool(q *PriorityQueue, maxWorkers int) *WorkerPool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &WorkerPool{
		queue:   q,
		workers: maxWorkers,
	}
}

// Start spins up the workers.
func (wp *WorkerPool) Start(ctx context.Context) {
	for i := 0; i < wp.workers; i++ {
		wp.wg.Add(1)
		go wp.workerLoop(ctx, i)
	}
}

func (wp *WorkerPool) workerLoop(ctx context.Context, id int) {
	defer wp.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		task, err := wp.queue.Pop(ctx)
		if err != nil {
			if err == context.Canceled {
				return
			}
			continue
		}

		// Execute the task
		if err := task.Execute(ctx); err != nil {
			log.Printf("Worker %d task %s failed: %v", id, task.ID, err)
		}
	}
}
