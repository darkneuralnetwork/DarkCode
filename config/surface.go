package config

// surface.go — one answer to "what can the user configure".
//
// There were four answers. The config type carried 49 fields, the HTTP API
// exposed ~19, the Settings tab rendered ~16 controls and the console offered 9
// commands — and they were different subsets. `plan_depth` reached the browser
// but not the console; `air_gap` and `cost_limit_*` reached neither and were not
// in the API at all; `temperature` and `max_concurrent` were settable over HTTP
// and invisible everywhere else.
//
// Nothing was broken by that, which is why it persisted. The cost was that every
// new setting needed three separate decisions and usually got one or two, and
// the failure mode is not hypothetical: `agentic_loop` existed in the config,
// the API, the browser AND the console, with the console's own command printing
// an apology for the config field.
//
// So the field metadata lives next to the field, and the surfaces are generated
// from it. Adding a setting is one decision again. A field with no descriptor
// here is not reachable from any interface, deliberately and visibly — see
// TestEveryConfigFieldIsDescribed, which fails when the two drift.

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Tier says how prominently a setting is offered.
type Tier string

const (
	// TierPrimary is one of the few things a user genuinely has to decide.
	// Keep this list short; it is the default view.
	TierPrimary Tier = "primary"
	// TierAdvanced exists in the file and the API, and appears in an interface
	// only behind an "Advanced" disclosure.
	TierAdvanced Tier = "advanced"
	// TierDerived is computed from something else (context length from the
	// model, routing mode from the registered count). Readable, not asked.
	TierDerived Tier = "derived"
)

// Field describes one setting well enough for any interface to render it
// without knowing what it means.
type Field struct {
	Name    string   `json:"name"`  // json tag, the wire name
	Label   string   `json:"label"` // human name
	Group   string   `json:"group"` // section heading
	Tier    Tier     `json:"tier"`
	Kind    string   `json:"kind"`              // string | bool | int | float | list | map
	Choices []string `json:"choices,omitempty"` // when the value is one of a fixed set
	Help    string   `json:"help,omitempty"`
	// Secret marks a value that must never be rendered or serialised back to
	// an interface. The redaction belongs next to the field rather than in
	// each renderer, which is how one of them ends up forgetting.
	Secret bool `json:"secret,omitempty"`
}

// The six primary settings, in the order they should be asked.
//
// Everything else is derived from the model, implied by the safety level,
// always-on because there is no reason to turn it off, or an advanced override.
// That is the whole reduction: not a single capability removed, but the default
// view matches the real decision count instead of the field count.
var descriptors = []Field{
	{Name: "models", Label: "Models", Group: "Models", Tier: TierPrimary, Kind: "map",
		Help: "Nothing works without one. Irreducible."},
	{Name: "safety_level", Label: "Safety", Group: "Autonomy", Tier: TierPrimary, Kind: "string",
		Choices: []string{"strict", "normal", "relaxed"},
		Help:    "How much autonomy to grant before asking. Genuinely personal."},
	{Name: "local_mode", Label: "Local model", Group: "Models", Tier: TierPrimary, Kind: "string",
		Choices: []string{"off", "auto", "on", "force"},
		Help:    "Whether to run offline. A constraint, not a preference."},
	{Name: "cost_limit_per_day_usd", Label: "Daily spend cap", Group: "Budget", Tier: TierPrimary, Kind: "float",
		Help: "Only you know your budget. 0 means no cap."},
	{Name: "air_gap", Label: "Air gap", Group: "Autonomy", Tier: TierPrimary, Kind: "bool",
		Help: "Refuse every connection that leaves the machine. Cannot be inferred."},
	{Name: "background_work", Label: "Background work", Group: "Autonomy", Tier: TierPrimary, Kind: "string",
		Choices: []string{"off", "light", "full"},
		Help:    "Whether the tool may use idle capacity on your machine."},

	// --- Advanced: real overrides, no default UI ---
	{Name: "model", Label: "Primary model", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "provider", Label: "Provider", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "base_url", Label: "Base URL", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "api_key", Label: "API key", Group: "Models", Tier: TierAdvanced, Kind: "string", Secret: true},
	{Name: "compressor_model", Label: "Compression model", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "embedding_model", Label: "Embedding model", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "memory_profile", Label: "Local context size", Group: "Models", Tier: TierAdvanced, Kind: "string",
		Choices: []string{"lean", "balanced", "max", ""}},
	{Name: "temperature", Label: "Temperature", Group: "Models", Tier: TierAdvanced, Kind: "float"},
	{Name: "max_turns", Label: "Max turns", Group: "Execution", Tier: TierAdvanced, Kind: "int"},
	{Name: "max_concurrent", Label: "Max concurrency", Group: "Execution", Tier: TierAdvanced, Kind: "int"},
	{Name: "execution_profile", Label: "Execution profile", Group: "Execution", Tier: TierAdvanced, Kind: "string",
		Choices: []string{"auto", "sequential", "parallel"}},
	{Name: "plan_approval", Label: "Approve plans", Group: "Execution", Tier: TierAdvanced, Kind: "string"},
	{Name: "plan_depth", Label: "Planning depth", Group: "Execution", Tier: TierAdvanced, Kind: "string"},
	{Name: "sandbox", Label: "Shell sandbox", Group: "Autonomy", Tier: TierAdvanced, Kind: "string",
		Choices: []string{"off", "auto", "on", "strict"}},
	{Name: "sandbox_writable", Label: "Extra writable paths", Group: "Autonomy", Tier: TierAdvanced, Kind: "list"},
	{Name: "deny_rules", Label: "Deny rules", Group: "Autonomy", Tier: TierAdvanced, Kind: "list"},
	{Name: "smart_approval", Label: "Auto-approve routine actions", Group: "Autonomy", Tier: TierAdvanced, Kind: "bool"},
	{Name: "approval_timeout_seconds", Label: "Approval timeout", Group: "Autonomy", Tier: TierAdvanced, Kind: "int"},
	{Name: "blast_radius_threshold", Label: "Blast-radius threshold", Group: "Autonomy", Tier: TierAdvanced, Kind: "float"},
	{Name: "execution_backend", Label: "Shell backend", Group: "Execution", Tier: TierAdvanced, Kind: "string",
		Choices: []string{"local", "docker", "ssh"}},
	{Name: "execution_image", Label: "Container image", Group: "Execution", Tier: TierAdvanced, Kind: "string"},
	{Name: "execution_host", Label: "SSH host", Group: "Execution", Tier: TierAdvanced, Kind: "string"},
	{Name: "execution_port", Label: "SSH port", Group: "Execution", Tier: TierAdvanced, Kind: "int"},
	{Name: "cost_limit_per_session_usd", Label: "Per-session spend cap", Group: "Budget", Tier: TierAdvanced, Kind: "float"},
	{Name: "cost_limit_action", Label: "On reaching the cap", Group: "Budget", Tier: TierAdvanced, Kind: "string"},
	{Name: "memory_dir", Label: "Memory directory", Group: "Storage", Tier: TierAdvanced, Kind: "string"},
	{Name: "projects_dir", Label: "Projects directory", Group: "Storage", Tier: TierAdvanced, Kind: "string"},
	{Name: "system_prompt", Label: "System prompt", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "tool_sources", Label: "Tool sources", Group: "Tools", Tier: TierAdvanced, Kind: "list"},
	{Name: "health_cpu_percent", Label: "Background CPU share", Group: "Autonomy", Tier: TierAdvanced, Kind: "int"},
	{Name: "local_model_role", Label: "Local model role", Group: "Models", Tier: TierAdvanced, Kind: "string"},
	{Name: "enable_local_offloading", Label: "Offload to local", Group: "Models", Tier: TierAdvanced, Kind: "bool"},
	{Name: "embedded_context_size", Label: "Embedded context size", Group: "Models", Tier: TierAdvanced, Kind: "int"},
	{Name: "embedded_idle_timeout_minutes", Label: "Embedded idle timeout", Group: "Models", Tier: TierAdvanced, Kind: "int"},
	{Name: "use_local_for_aux", Label: "Local model for aux calls", Group: "Models", Tier: TierAdvanced, Kind: "bool"},
	{Name: "skip_aux_for_read_only", Label: "Skip aux on read-only", Group: "Execution", Tier: TierAdvanced, Kind: "bool"},

	// --- Derived: readable, never asked ---
	{Name: "routing_mode", Label: "Routing mode", Group: "Models", Tier: TierDerived, Kind: "string",
		Help: "Follows from how many models are registered."},
	{Name: "context_length", Label: "Context length", Group: "Models", Tier: TierDerived, Kind: "int",
		Help: "Follows from the model."},
	{Name: "compress_context", Label: "Compress context", Group: "Execution", Tier: TierDerived, Kind: "bool",
		Help: "Always on: there is no reason to send more tokens than needed."},
	{Name: "use_ctx_engine", Label: "Context engine", Group: "Execution", Tier: TierDerived, Kind: "bool"},
	{Name: "health_daemon", Label: "Health daemon", Group: "Autonomy", Tier: TierDerived, Kind: "bool",
		Help: "Superseded by background_work; kept so old configs load."},
	{Name: "auto_ingest", Label: "Index the workspace", Group: "Autonomy", Tier: TierDerived, Kind: "bool",
		Help: "Superseded by background_work; kept so old configs load."},
	{Name: "enable_local_llm", Label: "Local LLM (legacy)", Group: "Models", Tier: TierDerived, Kind: "bool",
		Help: "Superseded by local_mode; kept so old configs load."},
	{Name: "ui_mode", Label: "UI mode", Group: "Storage", Tier: TierDerived, Kind: "bool"},
}

// Fields returns every described setting, primary first.
func Fields() []Field {
	out := make([]Field, len(descriptors))
	copy(out, descriptors)
	rank := map[Tier]int{TierPrimary: 0, TierAdvanced: 1, TierDerived: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Tier] != rank[out[j].Tier] {
			return rank[out[i].Tier] < rank[out[j].Tier]
		}
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// FieldsInTier returns the described settings at one tier.
func FieldsInTier(t Tier) []Field {
	var out []Field
	for _, f := range Fields() {
		if f.Tier == t {
			out = append(out, f)
		}
	}
	return out
}

// Described reports whether a wire name has a descriptor.
func Described(name string) bool {
	for _, f := range descriptors {
		if f.Name == name {
			return true
		}
	}
	return false
}

// JSONNames returns every json tag on Config, which is what the descriptors are
// checked against.
func JSONNames() []string {
	var out []string
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Values renders a config as wire-name → value, with secrets redacted.
//
// It goes through JSON so the names cannot disagree with what the API accepts;
// a reflection walk that computed names separately would be a fourth place for
// them to drift.
func Values(c *Config) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	for _, f := range descriptors {
		if !f.Secret {
			continue
		}
		if v, ok := out[f.Name]; ok {
			if sv, _ := v.(string); sv != "" {
				out[f.Name] = "••••"
			}
		}
	}

	// Report what is in EFFECT, not what happens to be stored.
	//
	// Two ways the raw struct misleads. A field that is empty because it is
	// inferred (background_work) marshals away under omitempty, so a primary
	// setting renders as unset while actually resolving to "full". And a legacy
	// field that has been superseded still holds its old value, so the derived
	// rows can contradict the canonical one directly above them.
	out["local_mode"] = c.ResolvedLocalMode()
	out["enable_local_llm"] = c.LocalEnabled()
	out["background_work"] = c.ResolvedBackgroundWork()
	out["auto_ingest"] = c.IngestInBackground()
	out["health_daemon"] = c.HealthDaemonEnabled()
	out["sandbox"] = c.ResolvedSandboxMode()

	return out
}
