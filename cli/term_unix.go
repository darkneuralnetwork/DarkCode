//go:build !windows

package cli

import (
	"syscall"
	"unsafe"
)

// terminalSize returns the width and height of the terminal, or (96, 24) if
// detection fails (e.g. piped output). Uses the TIOCGWINSZ ioctl directly so
// the project has no external terminal dependency.
func terminalSize() (int, int) {
	type winsize struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	var ws winsize
	for _, fd := range []uintptr{1, 0, 2} {
		ret, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
		if ret == 0 && errno == 0 && ws.Col > 0 {
			return int(ws.Col), int(ws.Row)
		}
	}
	return 96, 24
}
