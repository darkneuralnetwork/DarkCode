package llm

import (
	"strings"
	"testing"

	"github.com/darkcode/infra/config"
)

// TestContextWindowForRealModels. The heuristic this replaced gave 8,000 to
// every current cloud model, so the compression trigger was working from a
// figure 131× too small on the model most likely to be in use.
func TestContextWindowForRealModels(t *testing.T) {
	cases := map[string]int{
		"gemini-2.5-flash":         1048576,
		"gemini-2.5-pro":           1048576,
		"gemini-1.5-pro":           2097152,
		"claude-haiku-4-5-2025100": 200000,
		"claude-3-5-sonnet":        200000,
		"claude-sonnet-5":          1000000,
		"claude-sonnet-4-6":        1000000,
		"claude-haiku-4-5":         200000,
		"claude-fable-5":           1000000,
		"claude-opus-4-8":          1000000,
		"gpt-4o":                   128000,
		"gpt-4o-mini":              128000,
		"gpt-4o-2024-11-20":        128000,
		"o3":                       200000,
		"deepseek-chat":            65536,
		"llama-3.3-70b-instruct":   131072,
		"mistral-large-latest":     128000, // catalogue figure, not the pattern table's
		"mixtral-8x7b":             32768,
	}
	for model, want := range cases {
		if got := ContextWindowFor(model); got != want {
			t.Errorf("ContextWindowFor(%q) = %d, want %d", model, got, want)
		}
	}
}

// TestUnknownModelSaysSoRatherThanGuessing. Zero routes the caller to the
// configured value — a number a human chose — instead of one this table
// invented.
func TestUnknownModelSaysSoRatherThanGuessing(t *testing.T) {
	for _, m := range []string{"", "   ", "some-internal-model", "wizard-v9"} {
		if got := ContextWindowFor(m); got != 0 {
			t.Errorf("ContextWindowFor(%q) = %d, want 0 (unknown)", m, got)
		}
	}
}

// TestProviderPrefixDoesNotDefeatTheMatch. Aggregators route by "vendor/model",
// and the prefix must not turn a known model into an unknown one.
func TestProviderPrefixDoesNotDefeatTheMatch(t *testing.T) {
	pairs := [][2]string{
		{"google/gemini-2.5-flash", "gemini-2.5-flash"},
		{"openai/gpt-4o", "gpt-4o"},
		{"anthropic/claude-sonnet-4-5", "claude-sonnet-4-5"},
	}
	for _, p := range pairs {
		got, want := ContextWindowFor(p[0]), ContextWindowFor(p[1])
		if got != want || got == 0 {
			t.Errorf("ContextWindowFor(%q) = %d, want %d (same as unprefixed)", p[0], got, want)
		}
	}
}

// TestSpecificVariantsBeatTheirFamily. Ordering is the whole correctness
// argument for a substring table: "gpt-4" contains nothing that distinguishes
// it from "gpt-4o", so the narrower entry has to be reached first.
func TestSpecificVariantsBeatTheirFamily(t *testing.T) {
	if ContextWindowFor("gpt-4o") == ContextWindowFor("gpt-4") {
		t.Error("gpt-4o resolved to the plain gpt-4 window")
	}
	if ContextWindowFor("gpt-4") != 8192 {
		t.Errorf("gpt-4 = %d, want 8192", ContextWindowFor("gpt-4"))
	}
	if ContextWindowFor("gpt-4-32k") != 32768 {
		t.Errorf("gpt-4-32k = %d, want 32768", ContextWindowFor("gpt-4-32k"))
	}
	if ContextWindowFor("llama-3.3-70b") == ContextWindowFor("llama-3-8b") {
		t.Error("llama-3.3 resolved to the llama-3 window")
	}
}

// TestNoEntryIsShadowed. A fragment listed after one that contains it can never
// be reached, so it would be silently dead — the failure mode a substring table
// invites and the reason the order is asserted rather than assumed.
func TestNoEntryIsShadowed(t *testing.T) {
	for i, w := range windows {
		for j := 0; j < i; j++ {
			if strings.Contains(w.match, windows[j].match) {
				t.Errorf("entry %q (index %d) is unreachable: %q at index %d matches it first",
					w.match, i, windows[j].match, j)
			}
		}
	}
}

// TestSizeInTheNameIsUsedWhenNothingElseMatches. Self-hosted builds advertise
// the window in the id, and that is better evidence than this table has.
func TestSizeInTheNameIsUsedWhenNothingElseMatches(t *testing.T) {
	if got := ContextWindowFor("internal-model-128k"); got != 128*1024 {
		t.Errorf("internal-model-128k = %d, want %d", got, 128*1024)
	}
	if got := ContextWindowFor("something-32k-tuned"); got != 32*1024 {
		t.Errorf("something-32k-tuned = %d, want %d", got, 32*1024)
	}
	// "k" inside a word is not a size marker.
	if got := ContextWindowFor("kimi-thinking"); got != 0 {
		t.Errorf("kimi-thinking = %d, want 0", got)
	}
}

// TestModelInfoReportsTheWindow ties the table to the interface the compression
// trigger actually reads.
func TestModelInfoReportsTheWindow(t *testing.T) {
	c := &Client{Model: "gemini-2.5-flash"}
	if got := c.ModelInfo().Context; got != 1048576 {
		t.Errorf("ModelInfo().Context = %d, want 1048576", got)
	}
	// The old heuristic returned 8000 here, which is the bug.
	if c.ModelInfo().Context == 8000 {
		t.Error("still reporting the old hardcoded guess")
	}
	unknown := &Client{Model: "house-model-v2"}
	if got := unknown.ModelInfo().Context; got != 0 {
		t.Errorf("unknown model reports %d, want 0 so the caller uses the config", got)
	}
}

// TestCatalogWinsOverThePatternTable. The curated per-model list in config
// carries exact ids and is maintained alongside the pricing; the patterns here
// are the fallback for what it does not list. When both have an answer, the
// curated one is the better evidence.
func TestCatalogWinsOverThePatternTable(t *testing.T) {
	// mistral-large-latest is in the catalogue at 128000; the pattern entry for
	// "mistral-large" says 131072. The catalogue must decide.
	if got := ContextWindowFor("mistral-large-latest"); got != 128000 {
		t.Errorf("ContextWindowFor(mistral-large-latest) = %d, want the catalogue's 128000", got)
	}
	if got := config.CatalogContextWindow("mistral-large-latest"); got != 128000 {
		t.Fatalf("catalogue lookup returned %d; this test's premise is stale", got)
	}
	// A model the catalogue does not list still reaches the pattern table.
	if config.CatalogContextWindow("llama-3.3-70b-instruct") != 0 {
		t.Skip("the catalogue now lists llama-3.3; pick another uncatalogued model")
	}
	if got := ContextWindowFor("llama-3.3-70b-instruct"); got != 131072 {
		t.Errorf("uncatalogued model = %d, want the pattern table's 131072", got)
	}
}

// TestCurrentClaudeModelsHavePricing. The catalogue's newest Anthropic entry
// was Claude 3.5, so LookupPricing returned nothing for every model anyone
// actually runs, Record() left Cost at 0, and the spend cap never fired. A
// limit that silently does not apply is worse than no limit.
func TestCurrentClaudeModelsHavePricing(t *testing.T) {
	for _, m := range []string{
		"claude-fable-5", "claude-opus-4-8", "claude-sonnet-5", "claude-haiku-4-5",
	} {
		in, out, ok := config.LookupPricing("anthropic", m)
		if !ok {
			t.Errorf("no pricing for %q — cost would record 0 and the cap would never fire", m)
			continue
		}
		if in <= 0 || out <= 0 {
			t.Errorf("%q priced at %v/%v", m, in, out)
		}
	}
}

// TestCatalogAndPatternsAgreeOnTheWindow. Two tables that disagree is how the
// wrong one goes unnoticed; where both know a model, they must match.
func TestCatalogAndPatternsAgreeOnTheWindow(t *testing.T) {
	for _, m := range []string{
		"claude-fable-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
		"claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-4-5",
	} {
		catalog := config.CatalogContextWindow(m)
		if catalog == 0 {
			t.Errorf("%q is not in the catalogue", m)
			continue
		}
		// Temporarily bypass the catalogue by asking about a dated variant the
		// catalogue does not list, which forces the pattern table.
		pattern := ContextWindowFor(m + "-20260101")
		if pattern != catalog {
			t.Errorf("%q: catalogue says %d, pattern table says %d", m, catalog, pattern)
		}
	}
}
