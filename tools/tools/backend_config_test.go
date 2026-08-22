package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Choosing docker or ssh is a statement about where commands run. Quietly
// running them locally instead is the one outcome that must not happen, and it
// is what a fallback produces: the warning goes to stderr, which in GUI mode
// is a terminal nobody is reading.

func TestNewBackendRefusesAnUnknownKind(t *testing.T) {
	for _, kind := range []string{"dokcer", "podman", "vm", "remote"} {
		b, err := NewBackend(kind, "", "", 0, nil)
		if err == nil {
			t.Errorf("%q was accepted and produced %v", kind, b.Name())
		}
	}
}

// TestSSHWithoutAHostIsAnError. Half a configuration is not a configuration;
// defaulting the host would connect somewhere the user never named.
func TestSSHWithoutAHostIsAnError(t *testing.T) {
	if _, err := NewBackend("ssh", "", "", 0, nil); err == nil {
		t.Error("ssh with no host was accepted")
	}
	if _, err := NewBackend("ssh", "", "build.internal", 22, nil); err != nil {
		t.Errorf("a complete ssh config was rejected: %v", err)
	}
}

func TestLocalIsTheOnlyDefault(t *testing.T) {
	for _, kind := range []string{"", "local", "LOCAL", "  local  "} {
		b, err := NewBackend(kind, "", "", 0, nil)
		if err != nil {
			t.Fatalf("%q: %v", kind, err)
		}
		if b.Name() != "local" {
			t.Errorf("%q produced %q", kind, b.Name())
		}
	}
}

func TestDockerDefaultsOnlyTheImage(t *testing.T) {
	b, err := NewBackend("docker", "", "", 0, nil)
	if err != nil {
		t.Fatalf("docker with no image: %v", err)
	}
	if !strings.HasPrefix(b.Name(), "docker:") {
		t.Errorf("name = %q, want a docker backend", b.Name())
	}
	custom, err := NewBackend("docker", "alpine:3.20", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(custom.Name(), "alpine:3.20") {
		t.Errorf("the chosen image was dropped: %q", custom.Name())
	}
}

// TestMisconfiguredBackendRefusesInsteadOfRunningLocally is the point of the
// whole file: the failure mode must be a refusal the user sees, not a silent
// downgrade to running on their own machine.
func TestMisconfiguredBackendRefusesInsteadOfRunningLocally(t *testing.T) {
	tt := NewTerminalTool(nil)
	tt.Backend = MisconfiguredBackend{Err: errors.New("ssh backend requires execution_host")}

	res := tt.Execute(context.Background(), map[string]interface{}{"command": "echo ran-locally"})

	if res.Success {
		t.Fatal("a command ran despite an unusable backend")
	}
	if strings.Contains(res.Output, "ran-locally") {
		t.Fatal("the command executed locally as a fallback")
	}
	if !strings.Contains(res.Error, "execution_host") {
		t.Errorf("the refusal does not say what is wrong: %q", res.Error)
	}
	if !strings.Contains(res.Error, "NOT running locally") {
		t.Errorf("the refusal does not rule out a local fallback, which is the "+
			"assumption it exists to correct: %q", res.Error)
	}
}

// TestMisconfiguredBackendNeverProducesAWorkingCommand. Even reached through a
// path that skips the check, its argv must not succeed.
func TestMisconfiguredBackendNeverProducesAWorkingCommand(t *testing.T) {
	b := MisconfiguredBackend{Err: errors.New("boom")}
	argv := b.Argv("/tmp", "echo hello")
	if len(argv) == 0 {
		t.Fatal("empty argv")
	}
	for _, a := range argv {
		if strings.Contains(a, "echo hello") {
			t.Errorf("the requested command survived into argv: %v", argv)
		}
	}
	if b.Name() == "local" {
		t.Error("a misconfigured backend reports itself as local")
	}
}
