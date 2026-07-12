// Package strutil consolidates common string helpers used across the codebase.
// This eliminates the 6+ duplicate truncation/utility functions that were
// scattered across compression, memory, loop, permission, server, and kernel.
package strutil

import "strings"

// Truncate caps s to n characters and appends an ellipsis when truncated.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// TruncateEllipsis caps s to n characters and appends a Unicode ellipsis (…).
func TruncateEllipsis(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TruncateForPrompt caps s to maxChars for inclusion in an LLM prompt,
// adding a trailing "[truncated]" marker.
func TruncateForPrompt(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n…[truncated]"
}

// TruncateID returns the first max characters, with no suffix.
func TruncateID(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// NonEmpty returns the first non-empty string from the arguments.
func NonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// TruncateMid preserves the start and end of a string but truncates the middle.
func TruncateMid(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	half := maxBytes / 2
	return s[:half] + "\n...[truncated]...\n" + s[len(s)-half:]
}
