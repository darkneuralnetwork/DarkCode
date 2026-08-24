package tui

// theme.go — the same "dark orange" palette surfaces/cli/render.go uses,
// expressed as ANSI-256 color indices.
//
// Before this, this package hardcoded its own palette (#f97316 orange,
// #0ea5e9 blue, #10b981 green, ...) independently of render.go's — both
// called themselves "the dark orange theme," but the selector/input widgets
// this package renders (used by /help, /model, /always, and every setup
// wizard) picked a BLUE (#0ea5e9) as their primary accent, not orange, while
// every other CLI screen (banner, boxes, approval prompts) consistently
// accents in render.go's cOrange. tui can't import cli's render.go to reuse
// its constants directly — cli already imports tui, so that direction would
// cycle — so these are the same ANSI-256 indices restated here; keep them in
// sync if render.go's palette changes.
//
//	render.go     index  role here
//	cOrange (208) 208    colorAccent — primary accent (was blue, now orange)
//	cGreen  (41)  41     colorOK     — success / active state
//	cGray   (244) 244    colorMuted  — borders, descriptions, hints
//	cWhite  (255) 255    colorText   — primary readable text

import "github.com/charmbracelet/lipgloss"

var (
	colorAccent = lipgloss.Color("208")
	colorOK     = lipgloss.Color("41")
	colorMuted  = lipgloss.Color("244")
	colorText   = lipgloss.Color("255")
)
