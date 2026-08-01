package config

import (
	"encoding/json"
	"testing"
)

// TestLegacyLocalConfigStillLoads. The whole point of collapsing on write
// rather than migrating is that an existing config keeps its behaviour. A user
// who set enable_local_llm two versions ago must not silently lose their local
// model.
func TestLegacyLocalConfigStillLoads(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"legacy flag only", Config{EnableLocalLLM: true}, "auto"},
		{"neither set", Config{}, "off"},
		{"new field wins", Config{LocalMode: "force", EnableLocalLLM: false}, "force"},
		{"new field wins when off", Config{LocalMode: "off", EnableLocalLLM: true}, "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResolvedLocalMode(); got != tc.want {
				t.Errorf("ResolvedLocalMode = %q, want %q", got, tc.want)
			}
			wantEnabled := tc.want != "off"
			if got := tc.cfg.LocalEnabled(); got != wantEnabled {
				t.Errorf("LocalEnabled = %v, want %v", got, wantEnabled)
			}
		})
	}
}

// TestCanonicalWritesOneFieldPerQuestion. The written form should not carry the
// contradiction forward.
func TestCanonicalWritesOneFieldPerQuestion(t *testing.T) {
	cfg := &Config{EnableLocalLLM: true, AutoIngest: true, HealthDaemon: true}
	out := cfg.canonical()

	if out.LocalMode != "auto" {
		t.Errorf("local_mode = %q, want the resolved auto", out.LocalMode)
	}
	if out.EnableLocalLLM {
		t.Error("the legacy local flag is still written")
	}
	if out.BackgroundWork != BackgroundFull {
		t.Errorf("background_work = %q, want full", out.BackgroundWork)
	}
	if out.AutoIngest || out.HealthDaemon {
		t.Error("the legacy background flags are still written")
	}
}

// TestCanonicalDoesNotMutateTheLiveConfig. Zeroing the legacy fields in place
// would flip auto_ingest to false the moment any unrelated setting was saved —
// a live behaviour change from an operation that is supposed to be a no-op.
func TestCanonicalDoesNotMutateTheLiveConfig(t *testing.T) {
	cfg := &Config{EnableLocalLLM: true, AutoIngest: true, HealthDaemon: true}
	_ = cfg.canonical()

	if !cfg.EnableLocalLLM || !cfg.AutoIngest || !cfg.HealthDaemon {
		t.Error("canonical() mutated the config it was given")
	}
	if !cfg.IngestInBackground() {
		t.Error("saving turned background indexing off in the running process")
	}
}

// TestCanonicalIsIdempotent. Saving twice must not drift.
func TestCanonicalIsIdempotent(t *testing.T) {
	cfg := &Config{EnableLocalLLM: true, AutoIngest: true}
	once := cfg.canonical()
	twice := once.canonical()

	a, _ := json.Marshal(&once)
	b, _ := json.Marshal(&twice)
	if string(a) != string(b) {
		t.Errorf("canonical form drifts on a second save:\n%s\n%s", a, b)
	}
}

// TestCanonicalRoundTripPreservesBehaviour is the property that matters: write
// the canonical form, read it back, and every resolver answers the same.
func TestCanonicalRoundTripPreservesBehaviour(t *testing.T) {
	originals := []Config{
		{EnableLocalLLM: true, AutoIngest: true, HealthDaemon: true},
		{EnableLocalLLM: true, AutoIngest: true, HealthDaemon: false},
		{LocalMode: "force", AutoIngest: false, HealthDaemon: false},
		{},
	}
	for _, orig := range originals {
		out := orig.canonical()
		raw, err := json.Marshal(&out)
		if err != nil {
			t.Fatal(err)
		}
		var reloaded Config
		if err := json.Unmarshal(raw, &reloaded); err != nil {
			t.Fatal(err)
		}
		if got, want := reloaded.ResolvedLocalMode(), orig.ResolvedLocalMode(); got != want {
			t.Errorf("local mode changed across a save: %q → %q", want, got)
		}
		if got, want := reloaded.ResolvedBackgroundWork(), orig.ResolvedBackgroundWork(); got != want {
			t.Errorf("background work changed across a save: %q → %q", want, got)
		}
		if got, want := reloaded.IngestInBackground(), orig.IngestInBackground(); got != want {
			t.Errorf("background indexing changed across a save: %v → %v", want, got)
		}
		if got, want := reloaded.HealthDaemonEnabled(), orig.HealthDaemonEnabled(); got != want {
			t.Errorf("health daemon changed across a save: %v → %v", want, got)
		}
	}
}

// TestBackgroundWorkLevels pins the mapping the three old switches collapse to.
func TestBackgroundWorkLevels(t *testing.T) {
	cases := []struct {
		cfg          Config
		want         string
		ingest, heal bool
	}{
		{Config{BackgroundWork: "off"}, BackgroundOff, false, false},
		{Config{BackgroundWork: "light"}, BackgroundLight, true, false},
		{Config{BackgroundWork: "full"}, BackgroundFull, true, true},
		// Explicit level beats the legacy fields.
		{Config{BackgroundWork: "off", AutoIngest: true, HealthDaemon: true}, BackgroundOff, false, false},
		// No level: infer from what the old fields said.
		{Config{AutoIngest: true}, BackgroundLight, true, false},
		{Config{HealthDaemon: true}, BackgroundFull, true, true},
		{Config{}, BackgroundOff, false, false},
		// Junk falls back to inference rather than to a made-up level.
		{Config{BackgroundWork: "nonsense", AutoIngest: true}, BackgroundLight, true, false},
	}
	for _, c := range cases {
		if got := c.cfg.ResolvedBackgroundWork(); got != c.want {
			t.Errorf("%+v → %q, want %q", c.cfg.BackgroundWork, got, c.want)
		}
		if got := c.cfg.IngestInBackground(); got != c.ingest {
			t.Errorf("%q: IngestInBackground = %v, want %v", c.want, got, c.ingest)
		}
		if got := c.cfg.HealthDaemonEnabled(); got != c.heal {
			t.Errorf("%q: HealthDaemonEnabled = %v, want %v", c.want, got, c.heal)
		}
	}
}

// TestDefaultConfigKeepsBackgroundWorkOn. The default has always indexed the
// workspace and run the health daemon; the collapse must not quietly turn a
// fresh install into a passive one.
func TestDefaultConfigKeepsBackgroundWorkOn(t *testing.T) {
	d := DefaultConfig()
	if !d.IngestInBackground() {
		t.Error("a fresh config no longer indexes the workspace")
	}
	if !d.HealthDaemonEnabled() {
		t.Error("a fresh config no longer runs the health daemon")
	}
}

// TestValuesReportWhatIsInEffect. The raw struct misleads twice: a field left
// empty because it is inferred marshals away entirely, so a primary setting
// renders as unset; and a superseded legacy field keeps its old value, so the
// derived rows contradict the canonical one right above them.
func TestValuesReportWhatIsInEffect(t *testing.T) {
	// A legacy config: the canonical fields are absent, the old ones are set.
	cfg := &Config{EnableLocalLLM: true, AutoIngest: true, HealthDaemon: true}
	v := Values(cfg)

	if v["background_work"] != BackgroundFull {
		t.Errorf("background_work reads %v, want %q — an inferred primary "+
			"setting must not render as unset", v["background_work"], BackgroundFull)
	}
	if v["local_mode"] != "auto" {
		t.Errorf("local_mode reads %v, want auto", v["local_mode"])
	}
	if v["auto_ingest"] != true || v["health_daemon"] != true {
		t.Errorf("derived rows contradict background_work=full: %v / %v",
			v["auto_ingest"], v["health_daemon"])
	}

	// And the other direction: canonical set, legacy fields stale.
	stale := &Config{BackgroundWork: BackgroundOff, AutoIngest: true, HealthDaemon: true}
	sv := Values(stale)
	if sv["auto_ingest"] != false || sv["health_daemon"] != false {
		t.Errorf("stale legacy values shown over the canonical one: %v / %v",
			sv["auto_ingest"], sv["health_daemon"])
	}
}
