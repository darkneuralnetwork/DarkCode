package memory

import (
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// DEBOUNCED WRITER — Write-coalescing persistence layer
//
// Instead of writing the entire JSON file on EVERY mutation (which is O(N)
// serialization + synchronous fsync), the DebouncedWriter buffers mutations
// and flushes every few seconds. This reduces I/O from hundreds of writes
// per task to ~2-3.
//
// Usage:
//   writer := NewDebouncedWriter(path, 2*time.Second, serializeFunc)
//   writer.MarkDirty()  // called instead of os.WriteFile
//   writer.Shutdown()   // called on process exit (final flush)
// ============================================================================

// SerializeFunc is called by the writer to obtain the current data to persist.
// It must be safe to call concurrently (the writer acquires no external lock).
// The caller is responsible for ensuring the returned bytes are consistent.
type SerializeFunc func() ([]byte, error)

// DebouncedWriter batches file writes. When MarkDirty is called, it schedules
// a write after the debounce interval. Multiple MarkDirty calls within the
// interval coalesce into a single write.
type DebouncedWriter struct {
	path      string
	interval  time.Duration
	serialize SerializeFunc

	mu    sync.Mutex
	timer *time.Timer
	dirty int32 // atomic: 1 if dirty

	// stats for observability
	writes int64
	errors int64
}

// NewDebouncedWriter creates a writer that flushes at most once every interval.
func NewDebouncedWriter(path string, interval time.Duration, serialize SerializeFunc) *DebouncedWriter {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &DebouncedWriter{
		path:      path,
		interval:  interval,
		serialize: serialize,
	}
}

// MarkDirty signals that the in-memory state has changed and needs to be
// persisted. The actual write is deferred by the debounce interval.
func (w *DebouncedWriter) MarkDirty() {
	atomic.StoreInt32(&w.dirty, 1)

	w.mu.Lock()
	defer w.mu.Unlock()

	// If a timer is already running, let it fire (coalesce).
	if w.timer != nil {
		return
	}

	// Schedule a new flush.
	w.timer = time.AfterFunc(w.interval, func() {
		w.flush()
	})
}

// Shutdown performs a final synchronous flush and stops the timer.
// Must be called before process exit to avoid data loss.
func (w *DebouncedWriter) Shutdown() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()

	// Final flush if dirty.
	if atomic.LoadInt32(&w.dirty) != 0 {
		w.flush()
	}
}

// flush performs the actual write. Safe to call from the timer goroutine
// or from Shutdown.
func (w *DebouncedWriter) flush() {
	// Clear the timer reference so future MarkDirty calls schedule a new one.
	w.mu.Lock()
	w.timer = nil
	w.mu.Unlock()

	// Only write if actually dirty.
	if !atomic.CompareAndSwapInt32(&w.dirty, 1, 0) {
		return
	}

	data, err := w.serialize()
	if err != nil {
		atomic.AddInt64(&w.errors, 1)
		log.Printf("[memory] debounced writer: serialize error for %s: %v", w.path, err)
		// Re-mark dirty so we retry on next interval.
		atomic.StoreInt32(&w.dirty, 1)
		return
	}

	if err := os.WriteFile(w.path, data, 0644); err != nil {
		atomic.AddInt64(&w.errors, 1)
		log.Printf("[memory] debounced writer: write error for %s: %v", w.path, err)
		// Re-mark dirty so we retry on next interval.
		atomic.StoreInt32(&w.dirty, 1)
		return
	}

	atomic.AddInt64(&w.writes, 1)
}

// FlushNow forces an immediate synchronous flush, bypassing the debounce.
// Used when the caller needs durability guarantees (e.g., before returning
// from a critical operation).
func (w *DebouncedWriter) FlushNow() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	atomic.StoreInt32(&w.dirty, 1)
	w.flush()
}

// Stats returns write and error counts for observability.
func (w *DebouncedWriter) Stats() (writes, errors int64) {
	return atomic.LoadInt64(&w.writes), atomic.LoadInt64(&w.errors)
}
