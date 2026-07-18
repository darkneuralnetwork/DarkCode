package config

import "testing"

func TestMemoryProfileContext(t *testing.T) {
	cases := map[string]int{
		"lean":     8192,
		"balanced": 16384,
		"max":      32768,
		"Balanced": 16384, // case-insensitive
		" max ":    32768, // trimmed
		"":         0,      // auto
		"weird":    0,      // unknown → auto
	}
	for in, want := range cases {
		if got := MemoryProfileContext(in); got != want {
			t.Errorf("MemoryProfileContext(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestEffectiveEmbeddedContextSize(t *testing.T) {
	// Explicit override wins over the profile.
	c := &Config{EmbeddedContextSize: 12000, MemoryProfile: "lean"}
	if got := c.EffectiveEmbeddedContextSize(); got != 12000 {
		t.Errorf("explicit override should win: got %d, want 12000", got)
	}
	// No override → profile decides.
	c = &Config{MemoryProfile: "balanced"}
	if got := c.EffectiveEmbeddedContextSize(); got != 16384 {
		t.Errorf("profile balanced: got %d, want 16384", got)
	}
	// Neither → auto (0).
	c = &Config{}
	if got := c.EffectiveEmbeddedContextSize(); got != 0 {
		t.Errorf("no profile/override should be auto(0): got %d", got)
	}
	// Fresh install default is balanced.
	if got := DefaultConfig().EffectiveEmbeddedContextSize(); got != 16384 {
		t.Errorf("DefaultConfig should resolve to balanced 16384, got %d", got)
	}
}
