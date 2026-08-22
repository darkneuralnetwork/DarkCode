package config

import (
	"strings"
	"testing"
)

// TestEveryConfigFieldIsDescribed is the check that stops the divergence coming
// back. A field added to Config with no descriptor is unreachable from every
// interface, and the whole point of this file is that such a field cannot be
// added silently — it either gets a descriptor or it gets named here.
func TestEveryConfigFieldIsDescribed(t *testing.T) {
	// Fields that are structure rather than settings: they are read and written
	// by the program, never rendered as a control.
	notSettings := map[string]bool{
		"api_keys": true, // provider credential map, edited via the models UI
	}
	for _, name := range JSONNames() {
		if notSettings[name] || Described(name) {
			continue
		}
		t.Errorf("config field %q has no descriptor in surface.go — it would be "+
			"reachable from no interface at all", name)
	}
}

// TestNoDescriptorForAMissingField is the other direction: a descriptor for a
// field that no longer exists would render a control that writes nowhere.
func TestNoDescriptorForAMissingField(t *testing.T) {
	have := map[string]bool{}
	for _, n := range JSONNames() {
		have[n] = true
	}
	for _, f := range Fields() {
		if !have[f.Name] {
			t.Errorf("descriptor %q describes a field that is not in Config", f.Name)
		}
	}
}

// TestPrimaryTierStaysSmall. The default view is supposed to match the real
// decision count; "primary" quietly growing to twenty is how it stops doing
// that, and the growth is always individually reasonable.
func TestPrimaryTierStaysSmall(t *testing.T) {
	primary := FieldsInTier(TierPrimary)
	if len(primary) > 8 {
		var names []string
		for _, f := range primary {
			names = append(names, f.Name)
		}
		t.Errorf("%d primary settings: %s\nSix is the target. Moving one to advanced "+
			"is nearly always the right call.", len(primary), strings.Join(names, ", "))
	}
	if len(primary) == 0 {
		t.Error("no primary settings; the default view would be empty")
	}
}

// TestDescriptorsAreWellFormed — an interface renders these without knowing
// what they mean, so the metadata has to stand alone.
func TestDescriptorsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	kinds := map[string]bool{"string": true, "bool": true, "int": true, "float": true, "list": true, "map": true}
	tiers := map[Tier]bool{TierPrimary: true, TierAdvanced: true, TierDerived: true}
	for _, f := range Fields() {
		if seen[f.Name] {
			t.Errorf("duplicate descriptor for %q", f.Name)
		}
		seen[f.Name] = true
		if f.Label == "" {
			t.Errorf("%q has no label to render", f.Name)
		}
		if f.Group == "" {
			t.Errorf("%q has no group, so it cannot be placed in a section", f.Name)
		}
		if !kinds[f.Kind] {
			t.Errorf("%q has kind %q, which no interface knows how to render", f.Name, f.Kind)
		}
		if !tiers[f.Tier] {
			t.Errorf("%q has tier %q", f.Name, f.Tier)
		}
	}
}

// TestFieldsAreOrderedForRendering. Interfaces render this list directly, so
// the ordering is the layout.
func TestFieldsAreOrderedForRendering(t *testing.T) {
	fields := Fields()
	lastRank := -1
	rank := map[Tier]int{TierPrimary: 0, TierAdvanced: 1, TierDerived: 2}
	for _, f := range fields {
		if rank[f.Tier] < lastRank {
			t.Fatalf("%q (%s) came after a later tier", f.Name, f.Tier)
		}
		lastRank = rank[f.Tier]
	}
	if fields[0].Tier != TierPrimary {
		t.Error("the list does not lead with the settings a user has to decide")
	}
}

// TestValuesRedactsSecrets. The redaction lives with the field so no renderer
// has to remember it — this pins that it actually happens.
func TestValuesRedactsSecrets(t *testing.T) {
	v := Values(&Config{Model: "gpt-4o", APIKey: "sk-live-secret-value"})
	if got := v["api_key"]; got == "sk-live-secret-value" {
		t.Error("Values leaked the API key verbatim")
	}
	if v["model"] != "gpt-4o" {
		t.Errorf("Values mangled a non-secret field: %v", v["model"])
	}
	// An empty secret stays empty rather than becoming bullets, so an
	// interface can tell "unset" from "set but hidden".
	if got := Values(&Config{})["api_key"]; got != nil && got != "" {
		t.Errorf("an unset key rendered as %v, want empty", got)
	}
}

// TestValuesNamesMatchDescriptors — a value the descriptors cannot name is a
// value no interface can render.
func TestValuesNamesMatchDescriptors(t *testing.T) {
	v := Values(&Config{Model: "m", SafetyLevel: "normal", MaxTurns: 3})
	for name := range v {
		if name == "api_keys" {
			continue // credential map, not a setting
		}
		if !Described(name) {
			t.Errorf("Values emits %q, which has no descriptor", name)
		}
	}
}
