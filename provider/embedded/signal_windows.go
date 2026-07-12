//go:build windows

package embedded

import "os"

// On Windows there is no SIGTERM; use os.Kill (the only portable option).
var termSignal = os.Kill
