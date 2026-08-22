package security

import (
	"context"
	"strings"
	"testing"
)

// TestSandboxRealConfinement proves end-to-end that an active sandbox actually
// blocks writes outside the workspace. Skips where no backend is installed.
func TestSandboxRealConfinement(t *testing.T) {
	s := NewSandboxForMode(ModeAuto, nil, nil)
	if !s.Available() {
		t.Skip("no sandbox backend (bwrap/firejail) available")
	}
	ws := t.TempDir()
	if _, err := s.Run(context.Background(), ws, "bash", "-c", "echo hi > inside.txt"); err != nil {
		t.Fatalf("write inside the workspace should succeed: %v", err)
	}
	if _, err := s.Run(context.Background(), ws, "bash", "-c", "echo x > /etc/darkcode-should-not-write"); err == nil {
		t.Error("write to /etc should be blocked by the read-only sandbox")
	}
}

func containsSeq(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		if argv[i] == want[0] {
			match := true
			for j := range want {
				if argv[i+j] != want[j] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func TestSandboxWrapNoBackend(t *testing.T) {
	s := &Sandbox{Backend: BackendNone, Enabled: false}
	got := s.Wrap("/work", "bash", "-c", "echo hi")
	want := []string{"bash", "-c", "echo hi"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("no-backend Wrap = %v, want passthrough %v", got, want)
	}
}

func TestSandboxWrapBwrap(t *testing.T) {
	s := &Sandbox{Backend: BackendBwrap, binPath: "/usr/bin/bwrap", Enabled: true}
	argv := s.Wrap("/home/u/proj", "bash", "-c", "make")
	if argv[0] != "/usr/bin/bwrap" {
		t.Errorf("expected bwrap first, got %q", argv[0])
	}
	if !containsSeq(argv, "--ro-bind", "/", "/") {
		t.Error("bwrap wrap should mount / read-only")
	}
	if !containsSeq(argv, "--bind", "/home/u/proj", "/home/u/proj") {
		t.Error("bwrap wrap should bind the workspace writable")
	}
	if !containsSeq(argv, "bash", "-c", "make") {
		t.Error("bwrap wrap should end with the original command")
	}
}

func TestSandboxWrapFirejail(t *testing.T) {
	s := &Sandbox{Backend: BackendFirejail, binPath: "/usr/bin/firejail", Enabled: true}
	argv := s.Wrap("/home/u/proj", "bash", "-c", "make")
	if argv[0] != "/usr/bin/firejail" {
		t.Errorf("expected firejail first, got %q", argv[0])
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--read-only=/") {
		t.Error("firejail wrap should mount / read-only")
	}
	if !strings.Contains(joined, "--read-write=/home/u/proj") {
		t.Error("firejail wrap should allow writes to the workspace")
	}
}

func TestSandboxWrapEmptyWriteDir(t *testing.T) {
	s := &Sandbox{Backend: BackendBwrap, binPath: "/usr/bin/bwrap", Enabled: true}
	argv := s.Wrap("", "bash", "-c", "ls")
	if containsSeq(argv, "--bind") {
		t.Error("empty writeDir should produce a fully read-only sandbox (no --bind)")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{
		"off": ModeOff, "auto": ModeAuto, "on": ModeOn, "strict": ModeStrict,
		"": ModeAuto, "garbage": ModeAuto,
	} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSandboxMustRefuse(t *testing.T) {
	// strict + no backend must fail closed.
	strictNone := &Sandbox{Mode: ModeStrict, Backend: BackendNone}
	if !strictNone.MustRefuse() {
		t.Error("strict mode with no backend must refuse")
	}
	// strict + backend present does not refuse.
	strictOK := &Sandbox{Mode: ModeStrict, Backend: BackendBwrap, Enabled: true}
	if strictOK.MustRefuse() {
		t.Error("strict mode with a backend must not refuse")
	}
	// auto never refuses (best-effort).
	if (&Sandbox{Mode: ModeAuto, Backend: BackendNone}).MustRefuse() {
		t.Error("auto mode must never refuse")
	}
}

func TestSandboxModeOffPassthrough(t *testing.T) {
	// mode off => Enabled false => Wrap is a pass-through even with a backend.
	s := &Sandbox{Mode: ModeOff, Backend: BackendBwrap, binPath: "/usr/bin/bwrap", Enabled: false}
	if s.Available() {
		t.Error("mode off must not be Available")
	}
	got := s.Wrap("/work", "bash", "-c", "ls")
	if got[0] != "bash" {
		t.Errorf("mode off should pass through, got %v", got)
	}
}

func TestSandboxWritableBinds(t *testing.T) {
	s := &Sandbox{Backend: BackendBwrap, binPath: "/usr/bin/bwrap", Enabled: true,
		writable: []string{"/home/u/.cache"}}
	argv := s.Wrap("/home/u/proj", "bash", "-c", "make")
	if !containsSeq(argv, "--bind", "/home/u/.cache", "/home/u/.cache") {
		t.Error("configured writable dir should be bound writable")
	}
	if !containsSeq(argv, "--bind", "/home/u/proj", "/home/u/proj") {
		t.Error("workspace should still be bound writable")
	}
}
