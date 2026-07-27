// Package strutil holds the string helpers shared across the codebase, so the
// same shortening rules apply wherever text is trimmed for a prompt, a log line
// or an approval dialog.
//
// Every limit here is a byte budget, not a character count: callers use these
// to keep payloads under a size, and bytes are what a size is measured in.
// Cutting bytes blindly would split a multi-byte rune and emit invalid UTF-8 —
// which a provider API can reject and a terminal renders as a replacement
// character — so each cut backs up to the nearest rune boundary instead.
package strutil

import (
	"strings"
	"unicode/utf8"
)

// cut shortens s to at most n bytes, retreating to the nearest rune boundary
// so the result is always valid UTF-8.
func cut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Truncate caps s to n bytes, marking an elided tail with "...".
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return cut(s, n)
	}
	return cut(s, n-3) + "..."
}

// TruncateEllipsis caps s to n bytes and appends a Unicode ellipsis (…).
func TruncateEllipsis(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cut(s, n) + "…"
}

// TruncateForPrompt caps s to maxBytes for inclusion in an LLM prompt, leaving
// a marker so the model can tell the text was cut rather than ending there.
func TruncateForPrompt(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return cut(s, maxBytes) + "\n…[truncated]"
}

// TruncateID returns the first max bytes with no suffix, for building
// identifiers where a marker would become part of the key.
func TruncateID(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return cut(s, max)
}

// NonEmpty returns the first argument that holds something, treating a
// whitespace-only string as empty: these values are names and identifiers read
// from config, where a stray space is a typo rather than a choice, and falling
// through to the next candidate beats passing " " on to a provider.
func NonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// midMarker separates the two halves TruncateMid keeps.
const midMarker = "\n…[truncated]…\n"

// TruncateMid keeps the start and end of s and elides the middle, for content
// where both ends carry meaning — a plan whose heading and conclusion matter
// more than its centre.
//
// The head gets three quarters of the budget: when something has to go, the
// opening is more often the part worth keeping. The marker is charged against
// the budget so the result genuinely fits in maxBytes.
func TruncateMid(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Below the marker's own length there is no budget left to say anything
	// with, so a plain cut is the only honest result.
	if maxBytes <= len(midMarker) {
		return Truncate(s, maxBytes)
	}
	head := maxBytes * 3 / 4
	tail := maxBytes - head - len(midMarker)
	if tail < 0 {
		head, tail = maxBytes-len(midMarker), 0
	}
	return cut(s, head) + midMarker + tailCut(s, tail)
}

// tailCut returns the last n bytes of s, advancing to the nearest rune
// boundary so the fragment is valid UTF-8.
func tailCut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
