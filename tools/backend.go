package tools

// backend.go — where shell commands actually run.
//
// The default is the local machine, confined by the process sandbox. Two
// alternatives exist for work that should not touch the developer's machine at
// all: a throwaway Docker container, and a remote host over SSH. Selecting one
// changes nothing else — the same terminal tool, permission gate and approval
// flow apply, only the argv differs.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/darkcode/security"
)

// Backend turns a shell command into the argv that executes it.
type Backend interface {
	// Name identifies the backend for status output.
	Name() string
	// Argv returns the command line to run, given the workspace directory the
	// command should execute in.
	Argv(workDir, command string) []string
}

// LocalBackend runs commands on this machine, wrapped by the filesystem
// sandbox when one is available.
type LocalBackend struct{ Sandbox *security.Sandbox }

func (b LocalBackend) Name() string { return "local" }

func (b LocalBackend) Argv(workDir, command string) []string {
	argv := []string{"bash", "-c", command}
	if b.Sandbox != nil && b.Sandbox.Available() {
		return b.Sandbox.Wrap(workDir, argv[0], argv[1:]...)
	}
	return argv
}

// MisconfiguredBackend stands in for a backend that could not be built. Every
// command it is asked to run refuses with the configuration error.
//
// It exists because the alternative was worse. NewBackend deliberately errors
// on an unknown or incomplete backend rather than defaulting to local, and the
// caller then fell back to local anyway with a warning on stderr — which in
// GUI mode goes to a terminal nobody is reading. Someone who set
// execution_backend to "docker" and mistyped the image, or chose "ssh" without
// a host, believed their commands were running elsewhere while they ran on
// their own machine.
//
// Refusing matches how a strict sandbox with no backend already behaves: the
// terminal tool declines rather than running unconfined.
type MisconfiguredBackend struct{ Err error }

func (b MisconfiguredBackend) Name() string { return "misconfigured" }

// Argv is never reached — the terminal tool checks Err first — but returning a
// failing command rather than a working local one keeps the refusal true even
// if some future caller skips that check.
func (b MisconfiguredBackend) Argv(workDir, command string) []string {
	return []string{"false"}
}

// DockerBackend runs each command in a fresh container with the workspace
// bind-mounted. The container is disposable, drops all capabilities, and
// cannot gain new privileges — so a destructive command hurts the container,
// not the host.
type DockerBackend struct {
	Image string
	// Network, when empty, defaults to "none": most build and test commands
	// need no network, and denying it by default is the safer posture. Set to
	// "bridge" (or a named network) when dependency downloads are required.
	Network string
	// MemoryMB and PidsLimit bound a runaway process. Zero uses the defaults.
	MemoryMB  int
	PidsLimit int
}

func (b DockerBackend) Name() string { return "docker:" + b.Image }

func (b DockerBackend) Argv(workDir, command string) []string {
	network := b.Network
	if network == "" {
		network = "none"
	}
	memory := b.MemoryMB
	if memory <= 0 {
		memory = 2048
	}
	pids := b.PidsLimit
	if pids <= 0 {
		pids = 256
	}
	if workDir == "" {
		workDir = "/workspace"
	}
	return []string{
		"docker", "run", "--rm",
		"--network", network,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(pids),
		"--memory", strconv.Itoa(memory) + "m",
		"-v", workDir + ":" + workDir,
		"-w", workDir,
		b.Image, "bash", "-c", command,
	}
}

// SSHBackend runs commands on a remote host. BatchMode keeps ssh from
// blocking on an interactive password prompt, which would hang the agent;
// key-based auth is required.
type SSHBackend struct {
	Host string // [user@]hostname
	Port int
}

func (b SSHBackend) Name() string { return "ssh:" + b.Host }

func (b SSHBackend) Argv(workDir, command string) []string {
	remote := command
	if workDir != "" {
		remote = "cd " + shellQuote(workDir) + " && " + command
	}
	argv := []string{"ssh", "-o", "BatchMode=yes"}
	if b.Port > 0 {
		argv = append(argv, "-p", strconv.Itoa(b.Port))
	}
	return append(argv, b.Host, remote)
}

// NewBackend builds the configured execution backend. An unknown kind is an
// error rather than a silent fallback to local: "I thought it ran in Docker"
// is exactly the misunderstanding this feature exists to prevent.
func NewBackend(kind, image, host string, port int, sb *security.Sandbox) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "local":
		return LocalBackend{Sandbox: sb}, nil
	case "docker":
		if image == "" {
			image = "golang:1.24"
		}
		return DockerBackend{Image: image}, nil
	case "ssh":
		if host == "" {
			return nil, fmt.Errorf("ssh backend requires execution_host")
		}
		return SSHBackend{Host: host, Port: port}, nil
	default:
		return nil, fmt.Errorf("unknown execution backend %q (want local, docker, or ssh)", kind)
	}
}
