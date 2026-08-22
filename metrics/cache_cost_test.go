package metrics

import (
	"math"
	"testing"
)

// A request with cached prompt tokens must be cheaper than the same request
// billed at full input price, and the split must match input/cached/output
// pricing exactly. Uses the real catalogue entry (openai/gpt-4o: $2.50 in,
// $10 out per 1M) with the default cached rate = 50% of input.
func TestRecord_CachedTokensDiscountCost(t *testing.T) {
	u := NewUsageTracker()
	u.Record(RequestRecord{
		Provider:         "openai",
		Model:            "gpt-4o",
		PromptTokens:     1_000_000,
		CachedTokens:     800_000, // 80% of the prompt served from cache
		CompletionTokens: 100_000,
		Success:          true,
	})
	snap := u.Snapshot()

	inPrice, cachedPrice, outPrice := 2.50, 1.25, 10.00 // cached = 50% of input
	uncached := 200_000.0
	cached := 800_000.0
	completion := 100_000.0
	want := uncached/1e6*inPrice + cached/1e6*cachedPrice + completion/1e6*outPrice

	if math.Abs(snap.TotalCost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f (cached split not applied)", snap.TotalCost, want)
	}
	if snap.TotalCached != 800_000 {
		t.Errorf("TotalCached = %d, want 800000", snap.TotalCached)
	}

	// The saving must equal the cached tokens × (full − cached) input price.
	wantSaving := cached / 1e6 * (inPrice - cachedPrice)
	if math.Abs(snap.CacheSavings-wantSaving) > 1e-9 {
		t.Errorf("CacheSavings = %.6f, want %.6f", snap.CacheSavings, wantSaving)
	}
}

// With no cached tokens the cost is unchanged (full input price) — the split
// is a pure discount, never a surcharge.
func TestRecord_NoCacheIsFullPrice(t *testing.T) {
	u := NewUsageTracker()
	u.Record(RequestRecord{
		Provider:         "openai",
		Model:            "gpt-4o",
		PromptTokens:     1_000_000,
		CompletionTokens: 0,
		Success:          true,
	})
	snap := u.Snapshot()
	want := 1_000_000.0 / 1e6 * 2.50
	if math.Abs(snap.TotalCost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", snap.TotalCost, want)
	}
	if snap.CacheSavings != 0 {
		t.Errorf("CacheSavings = %.6f, want 0", snap.CacheSavings)
	}
}
