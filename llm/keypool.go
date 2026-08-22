package llm

import (
	"sync"
	"time"
)

// ============================================================================
// CREDENTIAL POOL
//
// Free and low-tier API keys are rate-limited per key, not per user, so a
// single key turns every burst into a 429 storm. A pool spreads calls across
// several keys and, when one is throttled, parks it for a cooldown and serves
// the next — the retry layer's next attempt then goes out on a fresh key
// instead of re-hitting the exhausted one.
// ============================================================================

// KeyPool hands out API keys round-robin, skipping keys that are cooling down
// after a rate-limit response. The zero value is unusable; use NewKeyPool.
type KeyPool struct {
	mu       sync.Mutex
	keys     []string
	cooldown map[string]time.Time
	next     int
}

// NewKeyPool builds a pool from the given keys, dropping blanks and
// duplicates. It returns nil when no usable key remains, so callers can treat
// "no pool" and "empty pool" identically.
func NewKeyPool(keys ...string) *KeyPool {
	seen := map[string]bool{}
	var clean []string
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		clean = append(clean, k)
	}
	if len(clean) == 0 {
		return nil
	}
	return &KeyPool{keys: clean, cooldown: map[string]time.Time{}}
}

// Len reports how many keys the pool holds.
func (p *KeyPool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// Get returns the next key that is not cooling down. When every key is cooling
// down it returns the one that recovers soonest — the caller still has to send
// something, and the retry layer's backoff covers the wait.
func (p *KeyPool) Get() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for range p.keys {
		k := p.keys[p.next%len(p.keys)]
		p.next++
		if until, parked := p.cooldown[k]; !parked || now.After(until) {
			delete(p.cooldown, k)
			return k
		}
	}
	soonest, best := p.keys[0], time.Time{}
	for _, k := range p.keys {
		if until := p.cooldown[k]; best.IsZero() || until.Before(best) {
			soonest, best = k, until
		}
	}
	return soonest
}

// Penalize parks a key for d after it was throttled or rejected, so the next
// call goes out on a different one.
func (p *KeyPool) Penalize(key string, d time.Duration) {
	if p == nil || key == "" || d <= 0 {
		return
	}
	p.mu.Lock()
	p.cooldown[key] = time.Now().Add(d)
	p.mu.Unlock()
}
