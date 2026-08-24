package cli

// ============================================================================
// RENDER TOOLKIT — ANSI colors, box drawing, sparklines, bar charts, spinners
//
// A small terminal-rendering library used by the interactive console and the
// live monitoring dashboard. Everything is pure functions returning strings,
// so it composes cleanly and is easy to test.
// ============================================================================

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/darkcode/memory/project"
)

// ---- Theme (matches the web UI orange/amber dark industrial palette) ----

const (
	clrReset  = "\033[0m"
	clrBold   = "\033[1m"
	clrDim    = "\033[2m"
	clrItalic = "\033[3m"
	clrUnder  = "\033[4m"

	// 256-color palette tuned for the dark orange theme.
	cOrange = "\033[38;5;208m" // primary accent
	cAmber  = "\033[38;5;214m" // secondary accent
	cBlue   = "\033[38;5;39m"
	cGreen  = "\033[38;5;41m"
	cYellow = "\033[38;5;221m"
	cRed    = "\033[38;5;203m"
	cPurple = "\033[38;5;141m"
	cCyan   = "\033[38;5;44m"
	cGray   = "\033[38;5;244m"
	cGrayLt = "\033[38;5;250m"
	cWhite  = "\033[38;5;255m"

	cBgPanel = "\033[48;5;234m"
	cBgHead  = "\033[48;5;236m"

	// box glyphs (unicode)
	tl = "╔" // top-left
	tr = "╗" // top-right
	bl = "╚" // bottom-left
	br = "╝" // bottom-right
	hz = "═" // horizontal double
	vt = "║" // vertical
	ml = "╠" // mid-left
	mr = "╣" // mid-right
	mc = "╬" // mid-cross
	hd = "═" // (same as hz)

	// single-line box glyphs (for inner panels)
	stl = "┌"
	str = "┐"
	sbl = "└"
	sbr = "┘"
	shz = "─"
	svt = "│"
	sml = "├"
	smr = "┤"
)

// colorOK caches the once-computed color-support check (supportsColor, in
// term_unix.go/term_windows.go). Checked once, not per call: on Windows this
// also has the side effect of enabling VT processing on the console, which
// only needs to happen once at startup, and repeating a syscall on every
// single paint() call across a busy render would be wasteful.
var (
	colorOnce sync.Once
	colorOK   bool
)

func colorEnabled() bool {
	colorOnce.Do(func() { colorOK = supportsColor() })
	return colorOK
}

// EnableTerminalColors performs the one-time terminal-capability check (and,
// on Windows, the SetConsoleMode call that turns on ANSI interpretation) as
// early as possible. Console mode is a process-wide setting, so calling this
// once at the very top of main() — before the interactive console, the
// first-run setup wizard, or any other startup message prints — fixes
// garbled ANSI escape codes for every print in the process, not just the
// ones that go through paint()/bold()/dim(). Safe to call multiple times
// (idempotent via sync.Once) and safe to skip (paint() etc. still gate
// correctly on first use if this is never called explicitly).
func EnableTerminalColors() {
	colorEnabled()
}

// paint wraps s in a color code + reset — unless the terminal can't render
// ANSI (NO_COLOR, redirected output, or a legacy Windows console that
// couldn't be switched into VT-processing mode), in which case s is
// returned unmodified so output degrades to plain text instead of garbled
// escape-code bytes (the Windows symptom this guards against).
func paint(c, s string) string {
	if !colorEnabled() {
		return s
	}
	return c + s + clrReset
}

// bold returns s in bold.
func bold(s string) string {
	if !colorEnabled() {
		return s
	}
	return clrBold + s + clrReset
}

// dim returns s in dim/grey.
func dim(s string) string {
	if !colorEnabled() {
		return s
	}
	return clrDim + s + clrReset
}

// ---- Terminal sizing ----

func termWidth() int {
	w, _ := terminalSize()
	if w < 40 {
		w = 96
	}
	return w
}

// ---- Number / cost / time formatting (mirrors the web UI) ----

func fmtNum(n int) string {
	return fmtNum64(int64(n))
}

// fmtAtoi parses an integer, returning an error on failure.
func fmtAtoi(s string) (int, error) {
	return strconv.Atoi(s)
}

func fmtNum64(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	}
	if n >= 1e6 {
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	}
	if n >= 1e3 {
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func fmtCost(c float64) string {
	if c == 0 {
		return "$0.00"
	}
	if c < 0.01 {
		return fmt.Sprintf("$%.4f", c)
	}
	return fmt.Sprintf("$%.2f", c)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "--:--:--"
	}
	return t.Format("15:04:05")
}

func fmtTimeShort(t time.Time) string {
	if t.IsZero() {
		return "--:--"
	}
	return t.Format("15:04")
}

func fmtDur(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// ---- Sparkline (unicode block histogram) ----

var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// sparkline renders a compact unicode histogram of the values, scaled to
// the block set. Empty input returns a dim placeholder.
func sparkline(values []float64, color string) string {
	if len(values) == 0 {
		return paint(cGray, strings.Repeat("·", 24))
	}
	min, max := values[0], values[0]
	for _, v := range values {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	if span == 0 {
		span = 1
	}
	var b strings.Builder
	for _, v := range values {
		idx := int((v - min) / span * float64(len(sparkBlocks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkBlocks) {
			idx = len(sparkBlocks) - 1
		}
		b.WriteRune(sparkBlocks[idx])
	}
	if color == "" {
		return b.String()
	}
	return paint(color, b.String())
}

// ---- Horizontal bar chart ----

// barRow renders a single horizontal bar:  label  ████████░░░░  value
// pct is 0..1. width is the total bar cell width.
func barRow(label string, value int, max int, width int, color string) string {
	if max <= 0 {
		max = 1
	}
	pct := float64(value) / float64(max)
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + paint(cGray, strings.Repeat("░", width-filled))
	lbl := label
	if len(lbl) > 18 {
		lbl = lbl[:15] + "…"
	}
	return fmt.Sprintf("  %s%s  %s  %s",
		paint(cGrayLt, padRight(lbl, 18)),
		bar,
		paint(cGray, fmtNum(value)),
		paint(cGray, fmt.Sprintf("(%d%%)", int(pct*100))),
	)
}

// padRight pads s to width (in runes, not bytes — a multi-byte glyph like the
// event icons in dashboard.go must count as one column, not three or four)
// with spaces.
func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// padLeft pads s on the left to width (in runes — see padRight).
func padLeft(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return strings.Repeat(" ", width-n) + s
}

// center centers s in width (in runes — see padRight).
func center(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	gap := width - n
	left := gap / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
}

// streamPreviewLines caps how many wrapped lines of the live streaming
// answer the spinner region shows (see liveRegion below) — enough to read
// actual sentences forming, bounded so a long answer doesn't scroll the
// visible terminal area away every tick (the redraw is in-place, not
// scrollback, so this cap is a viewport choice, not a memory one).
const streamPreviewLines = 5

// wrapText greedily word-wraps s to width-rune lines, then returns only the
// LAST maxLines of the result — the live preview should scroll forward as
// new tokens arrive, so the useful lines to keep are the most recent ones.
// Rune-safe (padRight's note applies here too: a multi-byte glyph counts as
// one column). A word longer than width is placed on its own line rather
// than split — this is a preview, not a hard column terminal.
func wrapText(s string, width, maxLines int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		lines = append(lines, cur.String())
		cur.Reset()
		curLen = 0
	}
	for _, word := range strings.Fields(s) {
		wl := utf8.RuneCountInString(word)
		if curLen > 0 && curLen+1+wl > width {
			flush()
		}
		if curLen > 0 {
			cur.WriteByte(' ')
			curLen++
		}
		cur.WriteString(word)
		curLen += wl
	}
	if curLen > 0 || len(lines) == 0 {
		flush()
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// ---- Answer markdown (final-answer formatting only) ----

// renderAnswerMarkdown lightly formats a final answer for terminal display:
// fenced code blocks become a bordered block with a language label, headings
// get the accent color, and inline **bold**/`code` spans get colored. This
// is not a CommonMark implementation — no dependency was added for it (see
// Phase 5 plan notes) — just enough structure that a multi-paragraph,
// code-containing answer reads as formatted rather than one grey wall of
// text. Applies only to the final printed answer; the live streaming
// preview (console.go's spinner tail) stays plain, since it is discarded
// wholesale the moment the real answer prints.
func renderAnswerMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				label := strings.TrimSpace(trimmed[3:])
				if label == "" {
					label = "code"
				}
				b.WriteString(paint(cGray, "  ┌─ "+label+" "+strings.Repeat("─", max(0, 40-utf8.RuneCountInString(label)))))
			} else {
				inFence = false
				b.WriteString(paint(cGray, "  └"+strings.Repeat("─", 44)))
			}
		} else if inFence {
			b.WriteString(paint(cGray, "  │ ") + paint(cCyan, line))
		} else {
			b.WriteString(renderInlineMarkdown(line))
		}
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderInlineMarkdown formats one non-fenced line: a leading "# "..."###### "
// run becomes a heading, and inline **bold**/`code` spans are colored.
// Delimiters are ASCII, so byte-indexed splitting here never cuts a
// multi-byte rune in half.
func renderInlineMarkdown(line string) string {
	if trimmed := strings.TrimLeft(line, "#"); trimmed != line {
		if n := len(line) - len(trimmed); n <= 6 && strings.HasPrefix(trimmed, " ") {
			return paint(cOrange+clrBold, strings.TrimSpace(trimmed))
		}
	}
	var b, plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			b.WriteString(paint(cWhite, plain.String()))
			plain.Reset()
		}
	}
	for i := 0; i < len(line); {
		switch {
		case strings.HasPrefix(line[i:], "**"):
			if end := strings.Index(line[i+2:], "**"); end >= 0 {
				flush()
				b.WriteString(paint(cWhite+clrBold, line[i+2:i+2+end]))
				i += 2 + end + 2
				continue
			}
		case line[i] == '`':
			if end := strings.IndexByte(line[i+1:], '`'); end >= 0 {
				flush()
				b.WriteString(paint(cCyan, line[i+1:i+1+end]))
				i += 1 + end + 1
				continue
			}
		}
		plain.WriteByte(line[i])
		i++
	}
	flush()
	return b.String()
}

// renderWorkflowTasks renders a project's structured workflow tasks as a
// status-badged list — the CLI counterpart of the GUI's checkbox board, both
// reading the same project.Workflow the store persists rather than each
// re-parsing workflow markdown their own way.
func renderWorkflowTasks(tasks []project.WorkflowTask) string {
	if len(tasks) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range tasks {
		badge, color := "TODO", cGray
		switch t.Status {
		case project.TaskDone:
			badge, color = "DONE", cGreen
		case project.TaskRunning:
			badge, color = "RUNNING", cAmber
		}
		fmt.Fprintf(&b, "  %s %s %s", paint(color+clrBold, "["+badge+"]"), paint(cOrange, t.ID+":"), paint(cWhite, t.Title))
		if i < len(tasks)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---- Box drawing ----

// box renders content (already multi-line) inside a double-line box of the
// given width. A title is rendered in the top border if non-empty.
func box(title, content string, width int) string {
	var b strings.Builder
	titleLine := ""
	if title != "" {
		inner := " " + paint(cOrange+clrBold, title) + " "
		titleLine = inner
	}
	// top
	b.WriteString(paint(cOrange, tl))
	if titleLine != "" {
		// place title at left after corner
		b.WriteString(titleLine)
		rem := width - 2 - visibleLen(titleLine)
		if rem > 0 {
			b.WriteString(paint(cOrange, strings.Repeat(hz, rem)))
		}
	} else {
		b.WriteString(paint(cOrange, strings.Repeat(hz, width-2)))
	}
	b.WriteString(paint(cOrange, tr) + "\n")

	for _, line := range strings.Split(content, "\n") {
		b.WriteString(paint(cOrange, vt))
		b.WriteString(line)
		rem := width - 2 - visibleLen(line)
		if rem > 0 {
			b.WriteString(strings.Repeat(" ", rem))
		}
		b.WriteString(paint(cOrange, vt) + "\n")
	}

	b.WriteString(boxBottom(width))
	return b.String()
}

// divider renders a horizontal rule of width w using box chars.
func divider(w int) string {
	return paint(cGray, strings.Repeat("─", w))
}

// boxDivider renders a double-line box-connecting mid-rule ("╠═══╣"), the
// companion to box()'s top/bottom border — the style dashboard.go's live
// monitoring panel uses between its sections. Named and shared here instead
// of the three identical ml+strings.Repeat(hz,...)+mr constructions that
// used to be typed out individually in dashboard.go.
func boxDivider(w int) string {
	return paint(cOrange, ml+strings.Repeat(hz, w-2)+mr)
}

// boxBottom renders the closing double-line border ("╚═══╝") — shared by
// box() and dashboard.go rather than each repeating the same three-piece
// construction.
func boxBottom(w int) string {
	return paint(cOrange, bl+strings.Repeat(hz, w-2)+br)
}

// ---- Rounded single-line prompt box (streaming) ----
//
// Unlike box(), which takes finished multi-line content and renders the
// whole thing in one call, this style is built incrementally — content
// lines print one at a time between roundedTop and roundedBottom as they
// become available (e.g. while waiting on interactive input, possibly
// looping to re-prompt), so it can't be a single pure function the way
// box() is. Used by the interactive approval prompt (console.go).

// roundedTop renders the top rule of a rounded box with an embedded title.
func roundedTop(title string, width int) string {
	inner := " " + title + " "
	rem := width - visibleLen(inner) - 2 // "╭─" prefix
	if rem < 0 {
		rem = 0
	}
	return "╭─" + inner + strings.Repeat("─", rem)
}

// roundedBottom renders the closing rule of a rounded box.
func roundedBottom(width int) string {
	if width < 1 {
		width = 1
	}
	return "╰" + strings.Repeat("─", width-1)
}

// roundedLine is the left-edge glyph for a content line inside a rounded box.
const roundedLine = "│"

// visibleLen returns the visible width of s in runes (stripping ANSI codes
// first) — byte length overcounts any multi-byte glyph, which box()'s
// padding math depends on being exact to keep its right border aligned.
func visibleLen(s string) int {
	return utf8.RuneCountInString(stripANSI(s))
}

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '\033' {
			in = true
			continue
		}
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---- ANSI cursor / screen control ----

const (
	ansiClearScreen = "\033[2J"
	ansiClearLine   = "\033[2K"
	ansiHome        = "\033[H"
	ansiHideCursor  = "\033[?25l"
	ansiShowCursor  = "\033[?25h"
	ansiSaveCursor  = "\033[s"
	ansiRestoreCur  = "\033[u"
)

func clearScreen() { fmt.Print(ansiClearScreen + ansiHome) }
func hideCursor()  { fmt.Print(ansiHideCursor) }

// cursorUp returns the escape sequence to move the cursor up n lines. n<=0
// is a no-op (empty string) rather than an ill-defined "\033[0A".
func cursorUp(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\033[%dA", n)
}

// liveRegion redraws a small block of terminal lines in place — the CLI's
// only "grows while you watch" surface. Everything else the CLI prints
// during a run (the ├─ event feed) is permanent scrollback and must never be
// touched once written; this is the opposite: nothing it draws is
// authoritative or meant to persist, so it's always safe to erase and
// redraw. It tracks how many lines it drew last time so every redraw knows
// exactly how far to move the cursor up first — the previous design's whole
// reason for staying single-line was the risk of a redraw miscounting how
// much of the terminal it owns; tracking that count here removes the risk
// instead of avoiding the feature.
type liveRegion struct {
	lastLines int
}

// redraw replaces whatever this region drew last with lines. The cursor must
// be sitting immediately after the region's own last redraw when this is
// called — i.e. nothing else may print to the terminal between two redraw
// calls (or a redraw and a clear) without going through this region, or its
// line-count bookkeeping desyncs from the real cursor position.
func (r *liveRegion) redraw(lines []string) {
	if r.lastLines > 0 {
		fmt.Print(cursorUp(r.lastLines))
	}
	n := len(lines)
	if r.lastLines > n {
		n = r.lastLines
	}
	for i := 0; i < n; i++ {
		fmt.Print("\r" + ansiClearLine)
		if i < len(lines) {
			fmt.Print(lines[i])
		}
		fmt.Print("\n")
	}
	if n > len(lines) {
		// This redraw was shorter than the last one: the loop above already
		// cleared the now-stale trailing lines, but printing all n of them
		// (rather than just len(lines)) left the cursor n lines below the
		// real content instead of len(lines) below it. Move back up to sit
		// right after the actual new content, same as every other redraw.
		fmt.Print(cursorUp(n - len(lines)))
	}
	r.lastLines = len(lines)
}

// clear erases the region and leaves the cursor where the region used to
// start — equivalent to redraw(nil), named for what it's used for at call
// sites (a real event superseding the preview, or the run ending).
func (r *liveRegion) clear() { r.redraw(nil) }
func showCursor()            { fmt.Print(ansiShowCursor) }

// ---- Spinner ----

type spinner struct {
	frames []string
	idx    int
}

func newSpinner() *spinner {
	return &spinner{frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}}
}

func (s *spinner) tick() string {
	f := s.frames[s.idx%len(s.frames)]
	s.idx++
	return f
}

// ---- progress bar (inline) ----

func progressBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	return paint(cOrange, strings.Repeat("█", filled)) + paint(cGray, strings.Repeat("░", width-filled))
}
