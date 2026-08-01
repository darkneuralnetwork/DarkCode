package config

// canonical.go — writing one field where two used to disagree.
//
// Some settings were asked twice. `enable_local_llm` and `local_mode` both said
// whether to run a local model, and the proof that this is redundant rather
// than merely verbose is that ResolvedLocalMode() exists: a function whose
// entire job is to decide what to do when the two disagree. Likewise
// `health_daemon`, `health_cpu_percent` and `auto_ingest` are three switches for
// one preference — whether the tool may use idle capacity on your machine.
//
// The fix is deliberately asymmetric. Every legacy field is still READ, so an
// existing config file loads with exactly the behaviour it had. Only the
// canonical field is WRITTEN, so a config that gets saved stops carrying the
// contradiction forward. Nothing changes for the user, and the redundancy
// drains out of real config files over time instead of needing a migration.

import "strings"

// BackgroundWork levels. One preference where there were three switches.
const (
	BackgroundOff   = "off"   // never touch the repo on our own
	BackgroundLight = "light" // keep indexes current, stay out of the way
	BackgroundFull  = "full"  // also run the health daemon at full share
)

// ResolvedBackgroundWork returns the effective background-work level.
//
// BackgroundWork wins when set. Otherwise it is inferred from the three fields
// it replaced, so an untouched config keeps behaving as it did.
func (cfg *Config) ResolvedBackgroundWork() string {
	switch strings.ToLower(strings.TrimSpace(cfg.BackgroundWork)) {
	case BackgroundOff, BackgroundLight, BackgroundFull:
		return strings.ToLower(strings.TrimSpace(cfg.BackgroundWork))
	}
	switch {
	case cfg.HealthDaemon:
		return BackgroundFull
	case cfg.AutoIngest:
		return BackgroundLight
	default:
		return BackgroundOff
	}
}

// IngestInBackground reports whether workspace indexing may run on its own.
func (cfg *Config) IngestInBackground() bool {
	return cfg.ResolvedBackgroundWork() != BackgroundOff
}

// HealthDaemonEnabled reports whether the repo-health daemon may run.
func (cfg *Config) HealthDaemonEnabled() bool {
	return cfg.ResolvedBackgroundWork() == BackgroundFull
}

// LocalEnabled reports whether a local model should be used at all.
//
// Callers want this rather than the raw EnableLocalLLM field, which is the
// legacy half of a two-field question and can disagree with local_mode.
func (cfg *Config) LocalEnabled() bool { return cfg.ResolvedLocalMode() != "off" }

// canonical returns a COPY of the config with the redundant fields collapsed
// onto the canonical one, for writing.
//
// A copy, not a mutation. Zeroing the legacy fields in place would be a live
// behaviour change for anything still reading them directly: the process would
// see auto_ingest flip to false the moment an unrelated setting was saved. What
// gets written is the resolved answer; what the program is holding is untouched.
//
// It is idempotent, and it never changes behaviour — every value it writes is
// what the resolver would have returned from the values it replaces.
func (cfg *Config) canonical() Config {
	out := *cfg

	// Local model: local_mode says everything enable_local_llm did.
	out.LocalMode = cfg.ResolvedLocalMode()
	out.EnableLocalLLM = false // legacy; still read on load, no longer written

	// Background work: one level instead of two booleans and a percentage.
	out.BackgroundWork = cfg.ResolvedBackgroundWork()
	out.AutoIngest = false
	out.HealthDaemon = false

	return out
}
