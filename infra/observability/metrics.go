package observability

import (
	"sync"
)

// MetricStore holds application metrics.
type MetricStore struct {
	mu           sync.Mutex
	TokenUsage   int64
	RequestCount int64
}

var globalMetrics = &MetricStore{}

// RecordTokens adds to the token counter.
func RecordTokens(count int64) {
	globalMetrics.mu.Lock()
	defer globalMetrics.mu.Unlock()
	globalMetrics.TokenUsage += count
}

// RecordRequest increments request counter.
func RecordRequest() {
	globalMetrics.mu.Lock()
	defer globalMetrics.mu.Unlock()
	globalMetrics.RequestCount++
}
