package cli

// ============================================================================
// TERMINAL RAW MODE
//
// Puts the terminal into cbreak/raw mode so the monitoring dashboard can read
// single keystrokes (q, r, Esc) without waiting for Enter. Uses the stty
// utility so the project has no external terminal dependency. If stdin is not
// a TTY (e.g. piped), the helpers are no-ops.
// ============================================================================

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// makeRaw switches the terminal to raw, no-echo mode and returns a restore
// function that returns it to its prior state. It is safe to call when stdin
// is not a TTY (in which case restore is a no-op).
func makeRaw() (restore func()) {
	// Capture the current terminal state so we can restore it exactly.
	get := exec.Command("stty", "-g")
	get.Stdin = os.Stdin
	saved, err := get.Output()
	if err != nil {
		return func() {}
	}
	state := strings.TrimSpace(string(saved))

	// Enter raw, no-echo mode.
	set := exec.Command("stty", "raw", "-echo")
	set.Stdin = os.Stdin
	if err := set.Run(); err != nil {
		return func() {}
	}

	return func() {
		reset := exec.Command("stty", state)
		reset.Stdin = os.Stdin
		_ = reset.Run()
		// Emit a newline so the prompt lands on a fresh line.
		fmt.Fprint(os.Stdout, "\n")
	}
}
