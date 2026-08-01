package cli

import (
	"strings"
	"testing"
	"time"
)

// These format the numbers in the usage and cost reports. A wrong figure here
// does not fail anything — it just tells the user something untrue about what
// they spent.

func TestFmtNum64(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},   // below 1k stays exact
		{1000, "1.0k"}, // the boundary abbreviates
		{1500, "1.5k"},
		{999999, "1000.0k"}, // still k just below 1M
		{1000000, "1.00M"},
		{2500000, "2.50M"},
		{1000000000, "1.00B"},
	}
	for _, tc := range cases {
		if got := fmtNum64(tc.in); got != tc.want {
			t.Errorf("fmtNum64(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFmtNumNeverLosesTheOrderOfMagnitude. Abbreviating is fine; abbreviating
// so hard that 2M and 200M look alike is not.
func TestFmtNumNeverLosesTheOrderOfMagnitude(t *testing.T) {
	seen := map[string]int64{}
	for _, n := range []int64{1, 999, 1000, 12345, 1000000, 250000000, 1000000000} {
		got := fmtNum64(n)
		if prev, dup := seen[got]; dup {
			t.Errorf("%d and %d both render as %q", prev, n, got)
		}
		seen[got] = n
	}
}

// TestFmtCostKeepsSmallAmountsVisible. On a cheap model a whole task can cost
// under a cent; rounding that to $0.00 would report every run as free.
func TestFmtCostKeepsSmallAmountsVisible(t *testing.T) {
	if got := fmtCost(0); got != "$0.00" {
		t.Errorf("fmtCost(0) = %q, want $0.00", got)
	}
	if got := fmtCost(0.0004); got == "$0.00" {
		t.Errorf("a real cost of $0.0004 rendered as %q — reads as free", got)
	}
	if got := fmtCost(0.0004); !strings.HasPrefix(got, "$0.0") {
		t.Errorf("fmtCost(0.0004) = %q", got)
	}
	// Above a cent, two decimals is the readable form.
	if got := fmtCost(1.239); got != "$1.24" {
		t.Errorf("fmtCost(1.239) = %q, want $1.24", got)
	}
}

// TestFmtTimeOnZeroValue. A zero timestamp means "never happened"; formatting
// it would print a real-looking time from year one.
func TestFmtTimeOnZeroValue(t *testing.T) {
	var zero time.Time
	if got := fmtTime(zero); got != "--:--:--" {
		t.Errorf("fmtTime(zero) = %q, want a placeholder", got)
	}
	if got := fmtTimeShort(zero); got != "--:--" {
		t.Errorf("fmtTimeShort(zero) = %q, want a placeholder", got)
	}

	at := time.Date(2026, 8, 1, 14, 5, 9, 0, time.UTC)
	if got := fmtTime(at); got != "14:05:09" {
		t.Errorf("fmtTime = %q, want 14:05:09", got)
	}
	if got := fmtTimeShort(at); got != "14:05" {
		t.Errorf("fmtTimeShort = %q, want 14:05", got)
	}
}

func TestPadding(t *testing.T) {
	if got := padRight("ab", 5); got != "ab   " {
		t.Errorf("padRight = %q", got)
	}
	if got := padLeft("ab", 5); got != "   ab" {
		t.Errorf("padLeft = %q", got)
	}
	// Over-long input is returned whole rather than cut — a truncated tool name
	// in a column is worse than a ragged column.
	if got := padRight("toolongforthecolumn", 5); got != "toolongforthecolumn" {
		t.Errorf("padRight truncated: %q", got)
	}
	if got := padLeft("toolongforthecolumn", 5); got != "toolongforthecolumn" {
		t.Errorf("padLeft truncated: %q", got)
	}
}

// TestProgressBarClampsOutOfRange. A percentage above 1 or below 0 must not
// produce a negative repeat count, which panics.
func TestProgressBarClampsOutOfRange(t *testing.T) {
	for _, pct := range []float64{-5, -0.01, 0, 0.5, 1, 1.01, 42} {
		got := progressBar(pct, 10) // must not panic
		if got == "" {
			t.Errorf("progressBar(%v) rendered nothing", pct)
		}
	}
}

func TestSplitFirstWord(t *testing.T) {
	cases := []struct{ in, wantHead, wantRest string }{
		{"model gpt-4o", "model", "gpt-4o"},
		{"single", "single", ""},
		{"  padded   rest here ", "padded", "  rest here"},
		{"", "", ""},
		{"tab\tseparated", "tab", "separated"},
	}
	for _, tc := range cases {
		head, rest := splitFirstWord(tc.in)
		if head != tc.wantHead || rest != tc.wantRest {
			t.Errorf("splitFirstWord(%q) = (%q, %q), want (%q, %q)",
				tc.in, head, rest, tc.wantHead, tc.wantRest)
		}
	}
}

func TestSplitCmd(t *testing.T) {
	got := splitCmd("  /tools   connect   mcp  ")
	want := []string{"/tools", "connect", "mcp"}
	if len(got) != len(want) {
		t.Fatalf("splitCmd = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
	if len(splitCmd("   ")) != 0 {
		t.Error("whitespace-only input produced arguments")
	}
}

// TestSafetyLabelRoundTrips. The label and the parser are two halves of one
// mapping; if they disagree, setting a level from its own displayed name
// changes it to something else.
func TestSafetyLabelRoundTrips(t *testing.T) {
	for level := 0; level <= 2; level++ {
		label := safetyLabel(level)
		if label == "" {
			t.Errorf("safety level %d has no label", level)
			continue
		}
		if got := parseSafetyInt(strings.ToLower(label)); got != level {
			t.Errorf("safetyLabel(%d) = %q, which parses back to %d", level, label, got)
		}
	}
}
