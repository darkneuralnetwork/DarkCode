//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// terminalSize returns the width and height of the terminal, or (96, 24) if
// detection fails. On Windows, we just return the default since TIOCGWINSZ is not available.
// If needed, we can use golang.org/x/sys/windows or golang.org/x/term here.
func terminalSize() (int, int) {
	return 96, 24
}

// enableVirtualTerminal turns on ANSI escape-sequence interpretation for
// stdout via SetConsoleMode + ENABLE_VIRTUAL_TERMINAL_PROCESSING. Without
// this, cmd.exe and legacy PowerShell consoles print raw \x1b[... bytes as
// literal text instead of colors/cursor movement — the garbled CLI output
// reported on some Windows systems. Returns true if VT processing is (now)
// enabled, false if the console doesn't support it (output should then fall
// back to plain, uncolored text — see supportsColor in render.go).
func enableVirtualTerminal() bool {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false // not a real console (piped/redirected) — no VT needed
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true // already enabled (Windows Terminal, modern PowerShell)
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false // legacy console that rejects the flag
	}
	return true
}

// supportsColor reports whether the current stdout can render ANSI color
// codes. On Windows this depends on successfully enabling VT processing.
func supportsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return enableVirtualTerminal()
}
