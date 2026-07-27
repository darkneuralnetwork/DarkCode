package tools

// fuzzy.go — tolerant matching for the patch tool.
//
// A model reproducing a snippet to edit routinely gets the indentation or the
// trailing whitespace slightly wrong. An exact-match-only patch rejects those,
// the model retries, and a turn is burnt re-reading a file it already had.
//
// The fallback here is deliberately narrow: whitespace may differ, nothing
// else, and the relaxed match must be UNIQUE. Patching the wrong region is far
// worse than failing to patch, so ambiguity is an error rather than a guess.

import (
	"strings"
)

// matchResult describes where a snippet was found.
type matchResult struct {
	start, end int    // byte offsets into the content
	fuzzy      bool   // true when whitespace had to be normalised
	err        string // set when no usable match exists
}

// findSnippet locates old within content, exactly if possible and otherwise by
// ignoring per-line leading/trailing whitespace.
func findSnippet(content, old string) matchResult {
	if idx := strings.Index(content, old); idx >= 0 {
		// Exact match. Ambiguity is fine here: the caller decides between
		// first-occurrence and replace-all, which is long-standing behaviour.
		return matchResult{start: idx, end: idx + len(old)}
	}

	oldLines := strings.Split(strings.Trim(old, "\n"), "\n")
	for i := range oldLines {
		oldLines[i] = strings.TrimSpace(oldLines[i])
	}
	if len(oldLines) == 0 {
		return matchResult{err: "old_string not found in file"}
	}

	// Line start offsets, so a window of lines maps back to byte offsets.
	lines := strings.Split(content, "\n")
	offsets := make([]int, len(lines)+1)
	pos := 0
	for i, l := range lines {
		offsets[i] = pos
		pos += len(l) + 1 // +1 for the newline
	}
	offsets[len(lines)] = pos

	var matches [][2]int
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		ok := true
		for j, want := range oldLines {
			if strings.TrimSpace(lines[i+j]) != want {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		end := offsets[i+len(oldLines)]
		if end > 0 && end <= len(content) && i+len(oldLines) < len(lines) {
			end-- // exclude the trailing newline we added
		}
		if end > len(content) {
			end = len(content)
		}
		matches = append(matches, [2]int{offsets[i], end})
	}

	switch len(matches) {
	case 0:
		return matchResult{err: "old_string not found in file (tried exact and whitespace-insensitive matching)"}
	case 1:
		return matchResult{start: matches[0][0], end: matches[0][1], fuzzy: true}
	default:
		// Silently picking one would be a coin flip over which region gets
		// rewritten.
		return matchResult{err: "old_string matches " + itoa(len(matches)) +
			" places when whitespace is ignored — include more surrounding context to disambiguate"}
	}
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
