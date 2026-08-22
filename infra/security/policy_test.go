package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPolicyMissingFileReturnsDefault(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing policy file should not error, got %v", err)
	}
	if p == nil || len(p.AllowedPaths) == 0 {
		t.Fatal("expected a usable default policy for a missing file")
	}
}

func TestLoadPolicyParsesJSON(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "security.json")
	if err := os.WriteFile(f, []byte(`{"allowed_paths":["src/*"],"denied_paths":["**/.env"],"network_mode":"none"}`), 0644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(f)
	if err != nil {
		t.Fatalf("LoadPolicy error: %v", err)
	}
	if p.NetworkMode != "none" {
		t.Errorf("NetworkMode = %q, want none", p.NetworkMode)
	}
}

func TestPolicyPathAllowed(t *testing.T) {
	p := &Policy{
		AllowedPaths: []string{"*"},
		DeniedPaths:  []string{"**/.ssh/**", "**/.env"},
	}
	cases := []struct {
		path string
		want bool
	}{
		{"project/main.go", true},
		{"project/src/app.js", true},
		{"home/user/.ssh/id_rsa", false},
		{".ssh/id_rsa", false},
		{"a/b/c/.env", false},
		{"config/.env", false},
	}
	for _, tc := range cases {
		if got := p.PathAllowed(tc.path); got != tc.want {
			t.Errorf("PathAllowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	// Empty allow-list denies everything (except when overridden by deny, which
	// also denies) — a fail-closed posture.
	empty := &Policy{}
	if empty.PathAllowed("anything") {
		t.Error("empty policy should deny by default")
	}
}
