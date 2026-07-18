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

// padRight pads s to width with spaces.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// padLeft pads s on the left to width.
func padLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// center centers s in width.
func center(s string, width int) string {
	if len(s) >= width {
		return s
	}
	gap := width - len(s)
	left := gap / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
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

	b.WriteString(paint(cOrange, bl))
	b.WriteString(paint(cOrange, strings.Repeat(hz, width-2)))
	b.WriteString(paint(cOrange, br))
	return b.String()
}

// divider renders a horizontal rule of width w using box chars.
func divider(w int) string {
	return paint(cGray, strings.Repeat("─", w))
}

// visibleLen returns the visible length of s (stripping ANSI codes).
func visibleLen(s string) int {
	out := stripANSI(s)
	return len(out)
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
func showCursor()  { fmt.Print(ansiShowCursor) }

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
