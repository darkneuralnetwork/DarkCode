//go:build windows

package tools

import (
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {
	// Setpgid is not available on Windows
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
