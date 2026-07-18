package config

import "testing"

// ResolvedLocalMode implements the never-force contract: LocalMode wins when
// valid, the legacy bool maps true→auto / false→off, and an unrecognized
// value can never resolve to "on" (a typo must not force-load a local model).
func TestResolvedLocalMode(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		legacy bool
		want   string
	}{
		{name: "explicit off", mode: "off", legacy: true, want: "off"},
		{name: "explicit auto", mode: "auto", legacy: false, want: "auto"},
		{name: "explicit on", mode: "on", legacy: false, want: "on"},
		{name: "explicit force", mode: "force", legacy: false, want: "force"},
		{name: "legacy enabled maps to auto", mode: "", legacy: true, want: "auto"},
		{name: "legacy disabled maps to off", mode: "", legacy: false, want: "off"},
		{name: "typo with legacy enabled degrades to auto", mode: "onn", legacy: true, want: "auto"},
		{name: "typo with legacy disabled degrades to off", mode: "always", legacy: false, want: "off"},
		{name: "force typo never force-loads", mode: "forcee", legacy: false, want: "off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{LocalMode: tt.mode, EnableLocalLLM: tt.legacy}
			if got := cfg.ResolvedLocalMode(); got != tt.want {
				t.Errorf("ResolvedLocalMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestForceLocal asserts ForceLocal() is true only for the resolved "force"
// mode — never for a typo (which degrades to off/auto) or the softer "on".
func TestForceLocal(t *testing.T) {
	tests := []struct {
		mode   string
		legacy bool
		want   bool
	}{
		{mode: "force", legacy: false, want: true},
		{mode: "on", legacy: false, want: false},
		{mode: "auto", legacy: true, want: false},
		{mode: "off", legacy: false, want: false},
		{mode: "forcee", legacy: false, want: false}, // typo must not force
	}
	for _, tt := range tests {
		cfg := &Config{LocalMode: tt.mode, EnableLocalLLM: tt.legacy}
		if got := cfg.ForceLocal(); got != tt.want {
			t.Errorf("ForceLocal(mode=%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}
