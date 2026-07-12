package cli

// ============================================================================
// DIFF & CHANGE RENDERING
//
// Renders the before → after state captured by the tools.ChangeRecorder so
// the user can see exactly which files were modified and what the previous vs
// new content was. Used by:
//   • the interactive console — a compact inline summary after each query
//   • the /log command — the full detailed trace
//   • the -q single-query mode — changes printed to stderr (stdout stays clean)
// ============================================================================

import (
	"fmt"
	"github.com/darkcode/internal/strutil"
	"io"
	"strings"

	"github.com/darkcode/core"
)

// splitLines splits s into lines without dropping a trailing empty element for
// a trailing newline (so a file ending in "\n" diffs cleanly).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline produces a final "" element; drop it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff computes a simple before → after diff. It strips the common prefix
// and suffix lines, then shows the changed middle region with '-' for removed
// lines and '+' for added lines, surrounded by a little context. This is ideal
// for patch-style edits; for full rewrites the middle region is large and gets
// truncated by maxLines.
func lineDiff(before, after string, maxLines int) string {
	b := splitLines(before)
	a := splitLines(after)

	// Common prefix.
	pre := 0
	for pre < len(b) && pre < len(a) && b[pre] == a[pre] {
		pre++
	}
	// Common suffix (not overlapping the prefix).
	suf := 0
	for suf < len(b)-pre && suf < len(a)-pre && b[len(b)-1-suf] == a[len(a)-1-suf] {
		suf++
	}

	var sb strings.Builder

	// Leading context (up to 2 lines).
	ctxStart := pre - 2
	if ctxStart < 0 {
		ctxStart = 0
	}
	for i := ctxStart; i < pre; i++ {
		sb.WriteString("  " + b[i] + "\n")
	}

	// Removed lines.
	remStart := pre
	remEnd := len(b) - suf
	for i := remStart; i < remEnd; i++ {
		sb.WriteString("- " + b[i] + "\n")
	}

	// Added lines.
	addStart := pre
	addEnd := len(a) - suf
	for i := addStart; i < addEnd; i++ {
		sb.WriteString("+ " + a[i] + "\n")
	}

	// Trailing context (up to 2 lines).
	tail := suf
	if tail > 2 {
		tail = 2
	}
	for i := 0; i < tail; i++ {
		sb.WriteString("  " + a[len(a)-suf+i] + "\n")
	}

	out := strings.TrimRight(sb.String(), "\n")
	return truncateDiffLines(out, maxLines)
}

// truncateDiffLines caps a diff to maxLines (0 = unlimited), appending a count
// notice when truncated.
func truncateDiffLines(s string, maxLines int) string {
	if maxLines <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n... (%d more lines truncated)", len(lines)-maxLines)
}

// changeKindLabel returns a short human label + icon for a change.
func changeKindLabel(c core.Change) (icon, label string) {
	switch c.Kind {
	case core.ChangeFileCreate:
		return "+", "created"
	case core.ChangeFileModify:
		return "✎", "modified"
	case core.ChangeFileDelete:
		return "✗", "deleted"
	case core.ChangeCommand:
		return "$", "ran"
	case core.ChangeGit:
		return "⎇", "git"
	default:
		return "•", string(c.Kind)
	}
}

// renderChange writes a single change to w. If fullDiff is true the entire
// before/after content is shown; otherwise the diff is capped to maxDiffLines.
func renderChange(w io.Writer, c core.Change, maxDiffLines int) {
	icon, label := changeKindLabel(c)

	switch {
	case c.IsFileChange():
		head := fmt.Sprintf("  %s %s  %s  %s",
			paint(cOrange, icon),
			paint(cWhite+clrBold, c.Path),
			paint(cGray, "("+label+")"),
			successTag(c.Success))
		fmt.Fprintln(w, head)
		diff := lineDiff(c.Before, c.After, maxDiffLines)
		if diff == "" {
			if c.Kind == core.ChangeFileCreate {
				fmt.Fprintln(w, paint(cGreen, "  + (new file)"))
			} else {
				fmt.Fprintln(w, paint(cGray, "  (no content)"))
			}
		} else {
			for _, line := range strings.Split(diff, "\n") {
				switch {
				case strings.HasPrefix(line, "- "):
					fmt.Fprintln(w, "  "+paint(cRed, line))
				case strings.HasPrefix(line, "+ "):
					fmt.Fprintln(w, "  "+paint(cGreen, line))
				default:
					fmt.Fprintln(w, "  "+paint(cGray, line))
				}
			}
		}
	case c.Kind == core.ChangeCommand:
		fmt.Fprintf(w, "  %s %s  %s\n",
			paint(cOrange, icon),
			paint(cWhite+clrBold, strutil.Truncate(c.Command, 80)),
			successTag(c.Success))
		if c.Output != "" {
			fmt.Fprintln(w, paint(cGray, indentBlock(strutil.Truncate(c.Output, 400), "    ")))
		}
	case c.Kind == core.ChangeGit:
		fmt.Fprintf(w, "  %s %s  %s\n",
			paint(cOrange, icon),
			paint(cWhite+clrBold, strutil.Truncate(c.Command, 80)),
			successTag(c.Success))
		if c.Output != "" {
			fmt.Fprintln(w, paint(cGray, indentBlock(strutil.Truncate(c.Output, 400), "    ")))
		}
	}
}

// successTag returns a colored ✓/✗ marker.
func successTag(ok bool) string {
	if ok {
		return paint(cGreen, "✓")
	}
	return paint(cRed, "✗ failed")
}

// indentBlock prefixes every line of s with prefix.
func indentBlock(s, prefix string) string {
	var sb strings.Builder
	for _, line := range strings.Split(s, "\n") {
		sb.WriteString(prefix)
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// PrintChanges writes a human-readable summary of the given changes to w.
// Exported so the non-interactive -q mode can print captured changes to
// stderr while keeping stdout a clean answer.
func PrintChanges(w io.Writer, changes []core.Change) {
	if len(changes) == 0 {
		return
	}
	fmt.Fprintln(w, paint(cAmber+clrBold, "▸ changes"))
	for _, c := range changes {
		renderChange(w, c, 30)
	}
	fmt.Fprintln(w)
}

// countFileChanges returns how many of the given changes modified files.
func countFileChanges(changes []core.Change) int {
	n := 0
	for _, c := range changes {
		if c.IsFileChange() {
			n++
		}
	}
	return n
}
