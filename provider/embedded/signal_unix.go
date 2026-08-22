//go:build !windows

package embedded

// signal_unix.go — the termination signal for graceful shutdown on Unix.
// On Windows we fall back to Kill (see signal_windows.go).

import "syscall"

var termSignal = syscall.SIGTERM
