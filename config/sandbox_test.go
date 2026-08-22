package config

import "testing"

func TestResolvedSandboxMode(t *testing.T) {
	// Config value is honored, unknown falls back to auto.
	for in, want := range map[string]string{
		"off": "off", "auto": "auto", "on": "on", "strict": "strict",
		"": "auto", "nonsense": "auto",
	} {
		cfg := &Config{Sandbox: in}
		if got := cfg.ResolvedSandboxMode(); got != want {
			t.Errorf("Sandbox=%q -> %q, want %q", in, got, want)
		}
	}
}

func TestResolvedSandboxModeEnvOverride(t *testing.T) {
	cfg := &Config{Sandbox: "off"}

	t.Setenv("DARKCODE_SANDBOX", "1")
	if got := cfg.ResolvedSandboxMode(); got != "on" {
		t.Errorf("DARKCODE_SANDBOX=1 should force on, got %q", got)
	}

	t.Setenv("DARKCODE_SANDBOX", "0")
	cfg.Sandbox = "strict"
	if got := cfg.ResolvedSandboxMode(); got != "off" {
		t.Errorf("DARKCODE_SANDBOX=0 should force off, got %q", got)
	}
}
