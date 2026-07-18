package security

import (
	"strings"
	"testing"
)

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
