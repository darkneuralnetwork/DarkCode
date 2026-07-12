//go:build windows

package cli

// terminalSize returns the width and height of the terminal, or (96, 24) if
// detection fails. On Windows, we just return the default since TIOCGWINSZ is not available.
// If needed, we can use golang.org/x/sys/windows or golang.org/x/term here.
func terminalSize() (int, int) {
	return 96, 24
}
