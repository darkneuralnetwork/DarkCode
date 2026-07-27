package tools

import (
	"strings"
	"testing"
)

func argvString(argv []string) string { return strings.Join(argv, " ") }

func TestLocalBackendRunsBash(t *testing.T) {
	argv := LocalBackend{}.Argv("/work", "go test ./...")
	if len(argv) != 3 || argv[0] != "bash" || argv[1] != "-c" || argv[2] != "go test ./..." {
		t.Errorf("argv = %v, want bash -c with the command intact", argv)
	}
}

// The container must be disposable and de-privileged, and must see the
// workspace at the same path so file references in output still make sense.
func TestDockerBackendHardening(t *testing.T) {
	argv := DockerBackend{Image: "golang:1.24"}.Argv("/home/u/proj", "go build ./...")
	got := argvString(argv)

	for _, want := range []string{
		"docker run --rm",
		"--network none",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--pids-limit 256",
		"--memory 2048m",
		"-v /home/u/proj:/home/u/proj",
		"-w /home/u/proj",
		"golang:1.24 bash -c go build ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
}

func TestDockerBackendOverrides(t *testing.T) {
	argv := argvString(DockerBackend{
		Image: "python:3.12", Network: "bridge", MemoryMB: 512, PidsLimit: 64,
	}.Argv("/w", "pytest"))
	for _, want := range []string{"--network bridge", "--memory 512m", "--pids-limit 64", "python:3.12"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
}

func TestSSHBackendQuotesWorkdirAndAvoidsPrompts(t *testing.T) {
	argv := SSHBackend{Host: "deploy@build-01", Port: 2222}.Argv("/srv/it's mine", "make")
	got := argvString(argv)

	if !strings.Contains(got, "-o BatchMode=yes") {
		t.Error("ssh must not block on an interactive password prompt")
	}
	if !strings.Contains(got, "-p 2222") {
		t.Errorf("port missing: %s", got)
	}
	// The final argument carries the remote command with a safely quoted cd.
	remote := argv[len(argv)-1]
	if !strings.HasPrefix(remote, `cd '/srv/it'\''s mine' && make`) {
		t.Errorf("remote command = %q, want a quoted cd then the command", remote)
	}
}

func TestSSHBackendWithoutWorkdirOrPort(t *testing.T) {
	argv := SSHBackend{Host: "host"}.Argv("", "uptime")
	got := argvString(argv)
	if strings.Contains(got, "-p ") {
		t.Errorf("no port configured but -p was passed: %s", got)
	}
	if argv[len(argv)-1] != "uptime" {
		t.Errorf("remote command = %q, want the bare command", argv[len(argv)-1])
	}
}

func TestNewBackendSelection(t *testing.T) {
	for _, kind := range []string{"", "local", "LOCAL", " local "} {
		b, err := NewBackend(kind, "", "", 0, nil)
		if err != nil || b.Name() != "local" {
			t.Errorf("NewBackend(%q) = %v, %v; want the local backend", kind, b, err)
		}
	}

	b, err := NewBackend("docker", "", "", 0, nil)
	if err != nil || !strings.HasPrefix(b.Name(), "docker:") {
		t.Errorf("docker backend = %v, %v", b, err)
	}

	b, err = NewBackend("ssh", "", "host", 22, nil)
	if err != nil || b.Name() != "ssh:host" {
		t.Errorf("ssh backend = %v, %v", b, err)
	}

	// Misconfiguration must be loud, never a silent downgrade to local.
	if _, err := NewBackend("ssh", "", "", 0, nil); err == nil {
		t.Error("ssh without a host should be an error")
	}
	if _, err := NewBackend("kubernetes", "", "", 0, nil); err == nil {
		t.Error("an unknown backend should be an error, not a local fallback")
	}
}
