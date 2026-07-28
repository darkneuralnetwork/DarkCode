package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicyFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The invariant the whole design rests on. If a policy could widen anything,
// dropping one next to the binary would be a privilege escalation rather than
// a restriction.
func TestPolicyOnlyEverRestricts(t *testing.T) {
	strictCfg := &Config{
		SafetyLevel:            "strict",
		ApprovalTimeoutSeconds: 30,
		BlastRadiusThreshold:   0.05,
		DenyRules:              []string{"terminal:*rm -rf*"},
	}
	// A policy asking for everything looser than the config already is.
	loose := Policy{Permissions: PermissionPolicy{
		MinSafetyLevel:            "off",
		MaxApprovalTimeoutSeconds: 9999,
		MaxBlastRadius:            0.99,
	}}
	loose.Apply(strictCfg)

	if strictCfg.SafetyLevel != "strict" {
		t.Errorf("safety level was relaxed to %q", strictCfg.SafetyLevel)
	}
	if strictCfg.ApprovalTimeoutSeconds != 30 {
		t.Errorf("approval timeout was extended to %d", strictCfg.ApprovalTimeoutSeconds)
	}
	if strictCfg.BlastRadiusThreshold != 0.05 {
		t.Errorf("blast radius was raised to %v", strictCfg.BlastRadiusThreshold)
	}
	if len(strictCfg.DenyRules) != 1 {
		t.Errorf("the user's own deny rule was disturbed: %v", strictCfg.DenyRules)
	}
}

func TestPolicyTightensWhatItCan(t *testing.T) {
	cfg := &Config{
		SafetyLevel:            "normal",
		ApprovalTimeoutSeconds: 600,
		BlastRadiusThreshold:   0.5,
		DenyRules:              []string{"browser"},
	}
	Policy{
		Tools: ToolPolicy{Deny: []string{"terminal:*curl*"}},
		Permissions: PermissionPolicy{
			MinSafetyLevel:            "strict",
			MaxApprovalTimeoutSeconds: 60,
			MaxBlastRadius:            0.1,
		},
	}.Apply(cfg)

	if cfg.SafetyLevel != "strict" {
		t.Errorf("safety level = %q, want strict", cfg.SafetyLevel)
	}
	if cfg.ApprovalTimeoutSeconds != 60 {
		t.Errorf("approval timeout = %d, want 60", cfg.ApprovalTimeoutSeconds)
	}
	if cfg.BlastRadiusThreshold != 0.1 {
		t.Errorf("blast radius = %v, want 0.1", cfg.BlastRadiusThreshold)
	}
	// Deny rules accumulate; a policy adds refusals and removes none.
	if len(cfg.DenyRules) != 2 {
		t.Errorf("deny rules = %v, want the user's plus the policy's", cfg.DenyRules)
	}
}

// Applying twice must not drift.
func TestApplyIsIdempotent(t *testing.T) {
	p := Policy{Permissions: PermissionPolicy{MaxApprovalTimeoutSeconds: 60, MaxBlastRadius: 0.1}}
	cfg := &Config{ApprovalTimeoutSeconds: 600, BlastRadiusThreshold: 0.5}
	p.Apply(cfg)
	first := *cfg
	p.Apply(cfg)
	if cfg.ApprovalTimeoutSeconds != first.ApprovalTimeoutSeconds ||
		cfg.BlastRadiusThreshold != first.BlastRadiusThreshold {
		t.Errorf("second apply changed the result: %+v vs %+v", *cfg, first)
	}
}

// An unset config field must still be tightened, or a default of zero would
// read as "no limit" and quietly escape the ceiling.
func TestPolicyTightensUnsetFields(t *testing.T) {
	cfg := &Config{}
	Policy{Permissions: PermissionPolicy{MaxApprovalTimeoutSeconds: 60, MaxBlastRadius: 0.2}}.Apply(cfg)
	if cfg.ApprovalTimeoutSeconds != 60 || cfg.BlastRadiusThreshold != 0.2 {
		t.Errorf("unset fields were left alone: %+v", *cfg)
	}
}

// --- models ---

func TestRequireLocalRefusesHostedProviders(t *testing.T) {
	p := Policy{Models: ModelPolicy{RequireLocal: true}}

	if ok, why := p.ModelAllowed("google", "gemini-2.5-flash"); ok {
		t.Error("a hosted provider passed a local-only policy")
	} else if !strings.Contains(why, "hosted") {
		t.Errorf("reason = %q, want it to name the problem", why)
	}
	if ok, _ := p.ModelAllowed("ollama", "llama3"); !ok {
		t.Error("a local provider was refused by a local-only policy")
	}
}

// "Unknown" is not an acceptable answer to "does this leave the machine".
func TestRequireLocalRefusesUnknownProviders(t *testing.T) {
	p := Policy{Models: ModelPolicy{RequireLocal: true}}
	if ok, _ := p.ModelAllowed("some-proxy", "anything"); ok {
		t.Error("an unrecognised provider passed a local-only policy")
	}
}

func TestModelDenyBeatsAllow(t *testing.T) {
	p := Policy{Models: ModelPolicy{
		Allow: []string{"google/*"},
		Deny:  []string{"google/gemini-2.5-pro"},
	}}
	if ok, _ := p.ModelAllowed("google", "gemini-2.5-flash"); !ok {
		t.Error("an allowed model was refused")
	}
	if ok, _ := p.ModelAllowed("google", "gemini-2.5-pro"); ok {
		t.Error("deny did not win over allow")
	}
}

func TestModelAllowListExcludesEverythingElse(t *testing.T) {
	p := Policy{Models: ModelPolicy{Allow: []string{"ollama/*"}}}
	if ok, why := p.ModelAllowed("openai", "gpt-4o"); ok {
		t.Error("a model outside the allow-list was permitted")
	} else if !strings.Contains(why, "allows only") {
		t.Errorf("reason = %q, want it to state the allow-list", why)
	}
}

func TestPriceCeilingRefusesExpensiveModels(t *testing.T) {
	// gemini-2.5-pro is listed at $1.25 in / $10.00 out.
	cheap := Policy{Models: ModelPolicy{MaxInputPrice: 0.20}}
	if ok, why := cheap.ModelAllowed("google", "gemini-2.5-pro"); ok {
		t.Error("a model above the input-price ceiling was permitted")
	} else if !strings.Contains(why, "input price") {
		t.Errorf("reason = %q, want it to name the price", why)
	}
	if ok, _ := cheap.ModelAllowed("google", "gemini-2.5-flash"); !ok {
		t.Error("a model under the ceiling was refused")
	}
}

// Refusing what cannot be priced would block every custom endpoint.
func TestUnpricedModelsPassAPriceCeiling(t *testing.T) {
	p := Policy{Models: ModelPolicy{MaxInputPrice: 0.01}}
	if ok, _ := p.ModelAllowed("my-custom-endpoint", "some-model"); !ok {
		t.Error("an unpriced model was refused by a price ceiling")
	}
}

func TestEmptyPolicyPermitsEverything(t *testing.T) {
	var p Policy
	if !p.Empty() {
		t.Error("a zero policy does not report itself empty")
	}
	if ok, _ := p.ModelAllowed("openai", "gpt-4o"); !ok {
		t.Error("an empty policy refused a model")
	}
	if ok, _ := p.ToolAllowed("terminal"); !ok {
		t.Error("an empty policy refused a tool")
	}
}

// --- tools ---

func TestToolAllowList(t *testing.T) {
	p := Policy{Tools: ToolPolicy{AllowOnly: []string{"read_file", "graph_*"}}}
	for tool, want := range map[string]bool{
		"read_file":   true,
		"graph_query": true,
		"terminal":    false,
		"write_file":  false,
	} {
		if ok, _ := p.ToolAllowed(tool); ok != want {
			t.Errorf("ToolAllowed(%q) = %v, want %v", tool, ok, want)
		}
	}
}

// --- loading ---

func TestLoadPolicyRoundTrip(t *testing.T) {
	p, err := LoadPolicy(writePolicyFile(t, `{
		"tools":       {"deny": ["browser"], "allow_only": ["read_file"]},
		"permissions": {"min_safety_level": "strict", "max_blast_radius": 0.1},
		"models":      {"require_local": true, "max_input_price": 0.5}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Empty() {
		t.Fatal("a populated policy reported itself empty")
	}
	if !p.Models.RequireLocal || p.Permissions.MinSafetyLevel != "strict" {
		t.Errorf("policy did not round-trip: %+v", p)
	}
}

func TestMissingPolicyFileIsEmpty(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Errorf("a missing policy was an error: %v", err)
	}
	if !p.Empty() {
		t.Error("a missing file produced restrictions")
	}
}

// A policy nobody can parse is not the same as no policy, and must not be
// treated as one.
func TestInvalidPolicyIsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"unknown safety level": `{"permissions":{"min_safety_level":"paranoid"}}`,
		"blast radius over 1":  `{"permissions":{"max_blast_radius":5}}`,
		"negative timeout":     `{"permissions":{"max_approval_timeout_seconds":-1}}`,
		"negative price":       `{"models":{"max_input_price":-2}}`,
		"malformed json":       `{"models":`,
		// The catalogue keys Google under "google"; "gemini/*" would match
		// nothing and silently permit everything on an allow-list.
		"unknown provider": `{"models":{"allow":["gemini/*"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPolicy(writePolicyFile(t, body)); err == nil {
				t.Error("an invalid policy loaded cleanly")
			}
		})
	}
}

func TestPolicyGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"google/*", "google/flash", true},
		{"google/*", "openai/gpt", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
	} {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}
