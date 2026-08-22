package strutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The bug these exist to prevent: every helper here caps a byte budget, and
// cutting bytes in the middle of a multi-byte rune produces invalid UTF-8.
// That reaches provider APIs as a malformed request body and terminals as a
// replacement character, and the content most likely to be multi-byte is
// exactly the untrusted text the injection scanner quotes.
func TestTruncationAlwaysProducesValidUTF8(t *testing.T) {
	inputs := []string{
		"修复缓存竞态条件——重构调度器",                 // CJK, 3 bytes per rune
		"café naïve résumé",               // 2-byte accents
		"emoji 🚀 payload 🔐 here",          // 4-byte runes
		"mixed ascii 中文 and ​ zero-width", // the injection-scanner case
	}
	for _, in := range inputs {
		for n := 0; n <= len(in)+2; n++ {
			for name, got := range map[string]string{
				"Truncate":          Truncate(in, n),
				"TruncateEllipsis":  TruncateEllipsis(in, n),
				"TruncateForPrompt": TruncateForPrompt(in, n),
				"TruncateID":        TruncateID(in, n),
				"TruncateMid":       TruncateMid(in, n),
			} {
				if !utf8.ValidString(got) {
					t.Fatalf("%s(%q, %d) = %q — not valid UTF-8", name, in, n, got)
				}
			}
		}
	}
}

// Truncating must never exceed the budget it was given, or the caller's
// context-window arithmetic is wrong in the dangerous direction.
func TestTruncateRespectsItsByteBudget(t *testing.T) {
	long := strings.Repeat("abcdefghij", 40)
	for _, n := range []int{0, 1, 3, 4, 17, 99, 250} {
		if got := Truncate(long, n); len(got) > n {
			t.Errorf("Truncate(_, %d) returned %d bytes, over budget", n, len(got))
		}
		if got := TruncateID(long, n); len(got) > n {
			t.Errorf("TruncateID(_, %d) returned %d bytes, over budget", n, len(got))
		}
		if got := TruncateMid(long, n); len(got) > n {
			t.Errorf("TruncateMid(_, %d) returned %d bytes, over budget", n, len(got))
		}
	}
}

// A tiny budget used to panic with "slice bounds out of range [:-1]".
func TestTruncateSurvivesTinyAndNegativeBudgets(t *testing.T) {
	for _, n := range []int{-5, -1, 0, 1, 2, 3} {
		Truncate("abcdef", n)
		TruncateEllipsis("abcdef", n)
		TruncateForPrompt("abcdef", n)
		TruncateID("abcdef", n)
		TruncateMid("abcdef", n)
	}
}

func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	const s = "short"
	for name, got := range map[string]string{
		"Truncate":          Truncate(s, 99),
		"TruncateEllipsis":  TruncateEllipsis(s, 99),
		"TruncateForPrompt": TruncateForPrompt(s, 99),
		"TruncateID":        TruncateID(s, 99),
		"TruncateMid":       TruncateMid(s, 99),
	} {
		if got != s {
			t.Errorf("%s did not pass a short string through: %q", name, got)
		}
	}
}

func TestTruncateMarksWhatItCut(t *testing.T) {
	long := strings.Repeat("x", 100)
	if got := Truncate(long, 20); !strings.HasSuffix(got, "...") {
		t.Errorf("Truncate should mark the elision: %q", got)
	}
	if got := TruncateEllipsis(long, 20); !strings.HasSuffix(got, "…") {
		t.Errorf("TruncateEllipsis should mark the elision: %q", got)
	}
	// TruncateID builds identifiers, where a marker would become part of the key.
	if got := TruncateID(long, 20); strings.ContainsAny(got, ".…") {
		t.Errorf("TruncateID must not add a marker: %q", got)
	}
}

// TruncateMid exists to keep both ends; the head is favoured deliberately.
func TestTruncateMidKeepsBothEnds(t *testing.T) {
	s := "HEAD" + strings.Repeat("-", 500) + "TAIL"
	got := TruncateMid(s, 120)
	if !strings.HasPrefix(got, "HEAD") {
		t.Errorf("lost the head: %q", got)
	}
	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("lost the tail: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("elision not marked: %q", got)
	}
}

func TestNonEmptyTreatsBlankAsEmpty(t *testing.T) {
	if got := NonEmpty("", "   ", "\t\n", "real"); got != "real" {
		t.Errorf("NonEmpty = %q, want the first value that holds something", got)
	}
	if got := NonEmpty("first", "second"); got != "first" {
		t.Errorf("NonEmpty = %q, want %q", got, "first")
	}
	if got := NonEmpty("", " "); got != "" {
		t.Errorf("NonEmpty of nothing usable = %q, want empty", got)
	}
}
