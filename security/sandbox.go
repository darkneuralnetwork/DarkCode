package security

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/darkcode/ui"
)

// Backend identifies the sandbox mechanism in use.
type Backend string

const (
	BackendNone     Backend = "none"     // no sandbox binary available
	BackendBwrap    Backend = "bubblewrap"
	BackendFirejail Backend = "firejail"
)

// Sandbox wraps process execution in a filesystem-confined environment. The
// confinement makes the whole filesystem read-only except an explicitly
// writable working directory, so a command cannot mutate files outside the
// project it is meant to touch. Network access is deliberately preserved so
// ordinary developer commands (go build, git, npm) still work — this is a
// blast-radius control for a local coding agent, not a network jail.
type Sandbox struct {
	Enabled bool
	Backend Backend
	binPath string
	emitter *ui.EventEmitter
}

// NewSandbox detects an available sandbox backend. Detection is real:
// bubblewrap is preferred (cleaner bind-mount model), firejail is the
// fallback. When neither is present the sandbox reports BackendNone and Wrap
// becomes a pass-through, so callers degrade gracefully instead of failing.
func NewSandbox(emitter *ui.EventEmitter) *Sandbox {
	s := &Sandbox{emitter: emitter}
	if p, err := exec.LookPath("bwrap"); err == nil {
		s.Backend, s.binPath, s.Enabled = BackendBwrap, p, true
	} else if p, err := exec.LookPath("firejail"); err == nil {
		s.Backend, s.binPath, s.Enabled = BackendFirejail, p, true
	} else {
		s.Backend, s.Enabled = BackendNone, false
	}
	return s
}

// Available reports whether a real sandbox backend was detected.
func (s *Sandbox) Available() bool {
	return s != nil && s.Enabled && s.Backend != BackendNone
}

// Wrap returns the argv that runs name+args confined so that only writeDir (and
// its subtree) is writable; the rest of the filesystem is read-only. If no
// backend is available it returns the command unchanged so execution still
// proceeds (best-effort). writeDir may be empty, in which case the whole
// filesystem is mounted read-only (fully read-only execution).
func (s *Sandbox) Wrap(writeDir, name string, args ...string) []string {
	base := append([]string{name}, args...)
	if !s.Available() {
		return base
	}
	switch s.Backend {
	case BackendBwrap:
		wrap := []string{
			s.binPath,
			"--ro-bind", "/", "/", // entire fs read-only …
			"--dev", "/dev",
			"--proc", "/proc",
			"--tmpfs", "/tmp",
			"--unshare-pid",
			"--die-with-parent",
		}
		if writeDir != "" {
			wrap = append(wrap, "--bind", writeDir, writeDir, "--chdir", writeDir) // … except the workspace
		}
		return append(wrap, base...)
	case BackendFirejail:
		wrap := []string{s.binPath, "--quiet", "--noprofile", "--read-only=/"}
		if writeDir != "" {
			wrap = append(wrap, "--read-write="+writeDir)
		}
		return append(wrap, base...)
	default:
		return base
	}
}

// Run executes name+args confined to writeDir and returns combined output.
func (s *Sandbox) Run(ctx context.Context, writeDir, name string, args ...string) ([]byte, error) {
	if s.emitter != nil && s.Available() {
		s.emitter.EmitTaskUpdate("security-sandbox", "executing",
			fmt.Sprintf("Running in %s sandbox: %s", s.Backend, name))
	}
	argv := s.Wrap(writeDir, name, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("sandbox execution failed: %w", err)
	}
	return out, nil
}
