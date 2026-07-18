package metrics

import (
	"sync"
	"time"

	"github.com/darkcode/config"
)

// ============================================================================
// USAGE TRACKER — Layer 7: Observability
//
// Accumulates token usage, estimated cost, and latency across every LLM
// call (streaming and non-streaming). Exposed to the monitoring dashboard
// via the /api/metrics/* endpoints and pushed live via SSE.
// ============================================================================

// RequestRecord captures a single LLM request.
type RequestRecord struct {
	ID               string    `json:"id"`
	Timestamp        time.Time `json:"timestamp"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	// CachedTokens is the subset of PromptTokens served from the provider's
	// prefix cache (billed at the cheaper cached rate). 0 when the provider
	// doesn't report caching.
	CachedTokens int     `json:"cached_tokens,omitempty"`
	TotalTokens  int     `json:"total_tokens"`
	Cost         float64 `json:"cost"`
	LatencyMs    int64   `json:"latency_ms"`
	Stream       bool    `json:"stream"`
	Success      bool    `json:"success"`
}

// ModelStat aggregates usage for a single model.
type ModelStat struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
}

// SeriesBucket is a per-minute aggregation of usage.
type SeriesBucket struct {
	Bucket           time.Time `json:"bucket"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Cost             float64   `json:"cost"`
	Requests         int       `json:"requests"`
	Errors           int       `json:"errors"`
}

// Snapshot is a point-in-time view of all usage data.
type Snapshot struct {
	Since           time.Time       `json:"since"`
	TotalPrompt     int             `json:"total_prompt_tokens"`
	TotalCompletion int             `json:"total_completion_tokens"`
	TotalCached     int             `json:"total_cached_tokens"`
	TotalTokens     int             `json:"total_tokens"`
	TotalCost       float64         `json:"total_cost"`
	// CacheSavings is the estimated USD not spent because TotalCached prompt
	// tokens were billed at the cached rate instead of full input price.
	CacheSavings    float64         `json:"cache_savings"`
	// TotalRequests is the number of LLM API calls. TotalTurns is the number of
	// user questions/messages. One turn fans out into several requests (routing,
	// answer, compression, skill extraction, …), so requests ÷ turns is the
	// average model calls per question — surfaced so the count isn't mistaken
	// for "one request per question".
	TotalRequests   int             `json:"total_requests"`
	TotalTurns      int             `json:"total_turns"`
	TotalErrors     int             `json:"total_errors"`
	AvgLatencyMs    float64         `json:"avg_latency_ms"`
	PerModel        []ModelStat     `json:"per_model"`
	Series          []SeriesBucket  `json:"series"`
	Recent          []RequestRecord `json:"recent"`
}

const (
	recentCap = 500
	seriesCap = 120 // 2 hours of per-minute buckets
)

// UsageTracker is a thread-safe accumulator of LLM usage metrics.
type UsageTracker struct {
	mu              sync.Mutex
	since           time.Time
	totalPrompt     int
	totalCompletion int
	totalCached     int
	totalTokens     int
	totalCost       float64
	cacheSavings    float64
	totalRequests   int
	totalTurns      int
	totalErrors     int
	totalLatencyMs  int64
	perModel        map[string]*ModelStat
	recent          []RequestRecord
	series          []SeriesBucket
	onRecord        func(RequestRecord)
}

// Default is the process-wide usage tracker.
var Default = NewUsageTracker()

// NewUsageTracker creates a fresh tracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{
		since:    time.Now(),
		perModel: make(map[string]*ModelStat),
		recent:   make([]RequestRecord, 0, recentCap),
	}
}

// SetOnRecord installs a callback fired after every recorded request.
// Used to push live token-usage events onto the SSE stream.
func (u *UsageTracker) SetOnRecord(f func(RequestRecord)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onRecord = f
}

// RecordTurn increments the user-turn (question) counter. Call once per
// user-submitted chat message, independent of how many LLM requests that turn
// fans out into.
func (u *UsageTracker) RecordTurn() {
	u.mu.Lock()
	u.totalTurns++
	u.mu.Unlock()
}

// Record logs a single LLM request, computing cost from the provider registry.
func (u *UsageTracker) Record(rec RequestRecord) {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	if rec.TotalTokens == 0 && (rec.PromptTokens != 0 || rec.CompletionTokens != 0) {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}
	// Cost from pricing registry; local/unknown models cost nothing. Prompt
	// tokens are split into cached (cheaper prefix-cache reads) and uncached,
	// so a stable, re-sent system prompt no longer gets charged full input
	// price — the cost meter reflects the caching saving instead of ignoring
	// it.
	if rec.Cost == 0 {
		in, cachedIn, out, ok := config.LookupPricingFull(rec.Provider, rec.Model)
		if ok {
			cached := rec.CachedTokens
			if cached > rec.PromptTokens {
				cached = rec.PromptTokens
			}
			uncached := rec.PromptTokens - cached
			rec.Cost = float64(uncached)/1e6*in +
				float64(cached)/1e6*cachedIn +
				float64(rec.CompletionTokens)/1e6*out
		}
	}

	key := rec.Provider + "/" + rec.Model

	u.mu.Lock()
	stat := u.perModel[key]
	if stat == nil {
		stat = &ModelStat{Model: rec.Model, Provider: rec.Provider}
		u.perModel[key] = stat
	}
	stat.PromptTokens += rec.PromptTokens
	stat.CompletionTokens += rec.CompletionTokens
	stat.TotalTokens += rec.TotalTokens
	stat.Cost += rec.Cost
	stat.Requests++
	if !rec.Success {
		stat.Errors++
	}

	u.totalPrompt += rec.PromptTokens
	u.totalCompletion += rec.CompletionTokens
	u.totalCached += rec.CachedTokens
	u.totalTokens += rec.TotalTokens
	u.totalCost += rec.Cost
	// Estimated saving: what the cached tokens would have cost at full input
	// price minus what they actually cost at the cached rate.
	if rec.CachedTokens > 0 {
		if in, cachedIn, _, ok := config.LookupPricingFull(rec.Provider, rec.Model); ok {
			u.cacheSavings += float64(rec.CachedTokens) / 1e6 * (in - cachedIn)
		}
	}
	u.totalRequests++
	if !rec.Success {
		u.totalErrors++
	}
	u.totalLatencyMs += rec.LatencyMs

	// Recent ring (keep newest)
	u.recent = append(u.recent, rec)
	if len(u.recent) > recentCap {
		u.recent = u.recent[len(u.recent)-recentCap:]
	}

	// Per-minute series bucket
	bucket := rec.Timestamp.Truncate(time.Minute)
	if n := len(u.series); n > 0 && u.series[n-1].Bucket.Equal(bucket) {
		b := &u.series[n-1]
		b.PromptTokens += rec.PromptTokens
		b.CompletionTokens += rec.CompletionTokens
		b.TotalTokens += rec.TotalTokens
		b.Cost += rec.Cost
		b.Requests++
		if !rec.Success {
			b.Errors++
		}
	} else {
		u.series = append(u.series, SeriesBucket{
			Bucket:           bucket,
			PromptTokens:     rec.PromptTokens,
			CompletionTokens: rec.CompletionTokens,
			TotalTokens:      rec.TotalTokens,
			Cost:             rec.Cost,
			Requests:         1,
			Errors:           boolToInt(!rec.Success),
		})
		if len(u.series) > seriesCap {
			u.series = u.series[len(u.series)-seriesCap:]
		}
	}

	onRecord := u.onRecord
	u.mu.Unlock()

	if onRecord != nil {
		onRecord(rec)
	}
}

// Snapshot returns a point-in-time copy of all usage data.
func (u *UsageTracker) Snapshot() Snapshot {
	u.mu.Lock()
	defer u.mu.Unlock()

	perModel := make([]ModelStat, 0, len(u.perModel))
	for _, s := range u.perModel {
		perModel = append(perModel, *s)
	}

	series := make([]SeriesBucket, len(u.series))
	copy(series, u.series)

	recent := make([]RequestRecord, len(u.recent))
	copy(recent, u.recent)

	var avg float64
	if u.totalRequests > 0 {
		avg = float64(u.totalLatencyMs) / float64(u.totalRequests)
	}

	return Snapshot{
		Since:           u.since,
		TotalPrompt:     u.totalPrompt,
		TotalCompletion: u.totalCompletion,
		TotalCached:     u.totalCached,
		TotalTokens:     u.totalTokens,
		TotalCost:       u.totalCost,
		CacheSavings:    u.cacheSavings,
		TotalRequests:   u.totalRequests,
		TotalTurns:      u.totalTurns,
		TotalErrors:     u.totalErrors,
		AvgLatencyMs:    avg,
		PerModel:        perModel,
		Series:          series,
		Recent:          recent,
	}
}

// Reset clears all accumulated metrics.
func (u *UsageTracker) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.since = time.Now()
	u.totalPrompt = 0
	u.totalCompletion = 0
	u.totalCached = 0
	u.totalTokens = 0
	u.totalCost = 0
	u.cacheSavings = 0
	u.totalRequests = 0
	u.totalTurns = 0
	u.totalErrors = 0
	u.totalLatencyMs = 0
	u.perModel = make(map[string]*ModelStat)
	u.recent = make([]RequestRecord, 0, recentCap)
	u.series = make([]SeriesBucket, 0, seriesCap)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
