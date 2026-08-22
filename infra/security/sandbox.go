package security

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/darkcode/surfaces/ui"
)

// Backend identifies the sandbox mechanism in use.
type Backend string

const (
	BackendNone     Backend = "none" // no sandbox binary available
	BackendBwrap    Backend = "bubblewrap"
	BackendFirejail Backend = "firejail"
)

// Mode controls how aggressively the sandbox is applied.
type Mode string

const (
	ModeOff    Mode = "off"    // never confine
	ModeAuto   Mode = "auto"   // confine when a backend exists, else run unconfined
	ModeOn     Mode = "on"     // confine; warn but run when no backend exists
	ModeStrict Mode = "strict" // require confinement; refuse to run without a backend
)

// ParseMode maps a config string to a Mode, defaulting to ModeAuto.
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModeOff, ModeAuto, ModeOn, ModeStrict:
		return Mode(s)
	default:
		return ModeAuto
	}
}

// Sandbox wraps process execution in a filesystem-confined environment: the
// filesystem is read-only except the workspace and a small set of build-cache
// directories, so a command can't mutate files outside the project it is meant
// to touch. Network access is preserved so ordinary developer commands still
// work — this is a blast-radius control for a local coding agent, not a
// network jail.
type Sandbox struct {
	Enabled  bool // a usable backend is present AND mode != off
	Backend  Backend
	Mode     Mode
	binPath  string
	writable []string // extra existing dirs kept writable beyond the per-call workspace
	emitter  *ui.EventEmitter
}

// NewSandbox detects a backend and runs in auto mode (confine when possible).
func NewSandbox(emitter *ui.EventEmitter) *Sandbox {
	return NewSandboxForMode(ModeAuto, nil, emitter)
}

// NewSandboxForMode detects a backend (bubblewrap preferred, firejail fallback)
// and configures the sandbox for the given mode. extraWritable lists additional
// absolute paths to keep writable (only those that exist are used).
func NewSandboxForMode(mode Mode, extraWritable []string, emitter *ui.EventEmitter) *Sandbox {
	s := &Sandbox{Mode: mode, emitter: emitter}
	detected := false
	if p, err := exec.LookPath("bwrap"); err == nil {
		s.Backend, s.binPath, detected = BackendBwrap, p, true
	} else if p, err := exec.LookPath("firejail"); err == nil {
		s.Backend, s.binPath, detected = BackendFirejail, p, true
	} else {
		s.Backend = BackendNone
	}
	s.Enabled = detected && mode != ModeOff
	s.writable = resolveWritable(extraWritable)
	return s
}

// resolveWritable returns the build-cache dirs (plus caller extras) that exist,
// so common toolchains that write to a cache don't fail under confinement while
// the rest of $HOME stays read-only.
func resolveWritable(extra []string) []string {
	var candidates []string
	if c := os.Getenv("XDG_CACHE_HOME"); c != "" {
		candidates = append(candidates, c)
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".cache"))
	}
	candidates = append(candidates, extra...)

	var out []string
	for _, d := range candidates {
		if d == "" {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil {
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				out = append(out, abs)
			}
		}
	}
	return out
}

// Available reports whether the sandbox will actually confine a command (a
// usable backend is present and the mode is not off).
func (s *Sandbox) Available() bool {
	return s != nil && s.Enabled && s.Backend != BackendNone
}

// MustRefuse reports whether a command must be refused rather than run
// unconfined: strict mode with no usable backend.
func (s *Sandbox) MustRefuse() bool {
	return s != nil && s.Mode == ModeStrict && !s.Available()
}

// Status is a one-line description for startup logging.
func (s *Sandbox) Status() string {
	if s == nil {
		return "sandbox: disabled"
	}
	return fmt.Sprintf("sandbox: mode=%s backend=%s active=%t", s.Mode, s.Backend, s.Available())
}

// Wrap returns the argv that runs name+args confined so only writeDir (and the
// configured writable dirs) can be written; the rest of the filesystem is
// read-only. With no usable backend it returns the command unchanged. writeDir
// may be empty, in which case only the writable dirs (if any) are writable.
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
		for _, d := range s.writable {
			wrap = append(wrap, "--bind", d, d)
		}
		if writeDir != "" {
			wrap = append(wrap, "--bind", writeDir, writeDir, "--chdir", writeDir) // … except the workspace
		}
		return append(wrap, base...)
	case BackendFirejail:
		wrap := []string{s.binPath, "--quiet", "--noprofile", "--read-only=/"}
		for _, d := range s.writable {
			wrap = append(wrap, "--read-write="+d)
		}
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
