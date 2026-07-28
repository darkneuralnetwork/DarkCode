package config

// policy.go — one file that says what this install is allowed to do.
//
// The settings that decide how much rope the agent has were spread across the
// config: deny rules here, an approval timeout there, a blast-radius threshold
// somewhere else, and nothing at all governing which models a repository's code
// may be sent to. That last gap is the one that matters — everything else can
// be reasoned about locally, while the choice of model decides whether source
// leaves the machine.
//
// A policy is a separate file on purpose. Config is something a user edits to
// make the tool convenient; a policy is something someone else may have set to
// make it safe, and the two want different lifetimes, different review, and
// different permissions on disk.
//
// The rule that makes the whole thing trustworthy: **a policy can only
// restrict.** It can forbid a tool the config allows, lower a timeout, or take
// a model away — it can never grant something the config withholds. Without
// that property, dropping a policy file next to the binary would be a
// privilege escalation rather than a restriction, and the safest-looking
// feature here would be the most dangerous.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Policy is the set of restrictions applied on top of a configuration.
type Policy struct {
	Tools       ToolPolicy       `json:"tools"`
	Permissions PermissionPolicy `json:"permissions"`
	Models      ModelPolicy      `json:"models"`
}

// ToolPolicy governs which tools may run at all.
type ToolPolicy struct {
	// Deny uses the same "tool" or "tool:pattern" form as the config's deny
	// rules, and is merged with them rather than replacing them.
	Deny []string `json:"deny,omitempty"`
	// AllowOnly, when non-empty, is an allow-list: any tool not named here is
	// refused. Names support a trailing "*".
	AllowOnly []string `json:"allow_only,omitempty"`
}

// PermissionPolicy governs how much passes without a human looking.
type PermissionPolicy struct {
	// MaxApprovalTimeoutSeconds caps how long an approval may block. A policy
	// can shorten the wait but never extend it.
	MaxApprovalTimeoutSeconds int `json:"max_approval_timeout_seconds,omitempty"`
	// MaxBlastRadius caps the configured escalation threshold, so a policy can
	// make central-file edits escalate sooner but not later.
	MaxBlastRadius float64 `json:"max_blast_radius,omitempty"`
	// MinSafetyLevel is the least strict level permitted: "strict" forbids
	// running at "normal" or "off".
	MinSafetyLevel string `json:"min_safety_level,omitempty"`
}

// ModelPolicy governs where code may be sent.
type ModelPolicy struct {
	// Allow, when non-empty, is an allow-list matched against the model name
	// and against "provider/model". A trailing "*" is a prefix match.
	Allow []string `json:"allow,omitempty"`
	// Deny is checked first and wins over Allow.
	Deny []string `json:"deny,omitempty"`
	// RequireLocal permits only self-hosted providers. This is the setting an
	// air-gapped or regulated install actually wants, and it is enforced on
	// the provider's own local flag rather than on a name, so a cloud endpoint
	// cannot pass by calling itself "ollama-proxy".
	RequireLocal bool `json:"require_local,omitempty"`
	// MaxInputPrice and MaxOutputPrice are ceilings in dollars per million
	// tokens. Zero means no ceiling. A model whose price is unknown is allowed:
	// refusing what we cannot price would block every custom endpoint.
	MaxInputPrice  float64 `json:"max_input_price,omitempty"`
	MaxOutputPrice float64 `json:"max_output_price,omitempty"`
}

// Empty reports whether the policy restricts nothing, so callers can skip the
// work entirely on the common path.
func (p Policy) Empty() bool {
	return len(p.Tools.Deny) == 0 && len(p.Tools.AllowOnly) == 0 &&
		p.Permissions == (PermissionPolicy{}) &&
		len(p.Models.Allow) == 0 && len(p.Models.Deny) == 0 &&
		!p.Models.RequireLocal &&
		p.Models.MaxInputPrice == 0 && p.Models.MaxOutputPrice == 0
}

// LoadPolicy reads a policy file. A missing file is an empty policy rather than
// an error: most installs have none, and that is a valid state.
func LoadPolicy(path string) (Policy, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Policy{}, nil
		}
		return Policy{}, err
	}
	var p Policy
	if err := json.Unmarshal(blob, &p); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// safetyRank orders the levels from most to least permissive, so a minimum can
// be compared. "off" is not a configurable level but is accepted here because
// a policy that forbids it should be able to say so.
var safetyRank = map[string]int{"off": 0, "normal": 1, "strict": 2}

func (p Policy) validate() error {
	if l := p.Permissions.MinSafetyLevel; l != "" {
		if _, ok := safetyRank[l]; !ok {
			return fmt.Errorf("unknown min_safety_level %q (want off, normal, strict)", l)
		}
	}
	if p.Permissions.MaxBlastRadius < 0 || p.Permissions.MaxBlastRadius > 1 {
		return fmt.Errorf("max_blast_radius is a fraction between 0 and 1, got %v",
			p.Permissions.MaxBlastRadius)
	}
	if p.Permissions.MaxApprovalTimeoutSeconds < 0 {
		return fmt.Errorf("max_approval_timeout_seconds cannot be negative")
	}
	if p.Models.MaxInputPrice < 0 || p.Models.MaxOutputPrice < 0 {
		return fmt.Errorf("price ceilings cannot be negative")
	}
	for _, pattern := range append(append([]string{}, p.Models.Allow...), p.Models.Deny...) {
		if err := checkProviderPrefix(pattern); err != nil {
			return err
		}
	}
	return nil
}

// checkProviderPrefix rejects a model pattern whose provider does not exist.
//
// The catalogue keys Google's models under "google", so a policy written as
// "gemini/*" — which is what anyone would reach for — matches nothing. On an
// allow-list that silently permits every model instead of the intended few,
// which is the worst possible way for a security setting to be wrong. A typo
// here fails at load rather than at the moment it matters.
func checkProviderPrefix(pattern string) error {
	prefix, _, ok := strings.Cut(pattern, "/")
	if !ok || prefix == "" || prefix == "*" || strings.HasSuffix(prefix, "*") {
		return nil // no provider named, or a wildcard that spans providers
	}
	if _, known := LookupProvider(prefix); !known {
		return fmt.Errorf("model pattern %q names unknown provider %q — "+
			"use a provider id from the catalogue, or drop the prefix to match on model name alone",
			pattern, prefix)
	}
	return nil
}

// Apply tightens cfg in place. It never loosens: each field moves only in the
// restrictive direction, so applying a policy twice, or applying a weaker one
// after a stronger one, cannot give anything back.
func (p Policy) Apply(cfg *Config) {
	if cfg == nil || p.Empty() {
		return
	}

	// Deny rules accumulate. A policy adds refusals; it cannot delete the
	// user's own.
	cfg.DenyRules = append(cfg.DenyRules, p.Tools.Deny...)

	if max := p.Permissions.MaxApprovalTimeoutSeconds; max > 0 {
		if cfg.ApprovalTimeoutSeconds == 0 || cfg.ApprovalTimeoutSeconds > max {
			cfg.ApprovalTimeoutSeconds = max
		}
	}
	if max := p.Permissions.MaxBlastRadius; max > 0 {
		if cfg.BlastRadiusThreshold == 0 || cfg.BlastRadiusThreshold > max {
			cfg.BlastRadiusThreshold = max
		}
	}
	if min := p.Permissions.MinSafetyLevel; min != "" {
		if safetyRank[cfg.SafetyLevel] < safetyRank[min] {
			cfg.SafetyLevel = min
		}
	}
}

// ToolAllowed reports whether a tool may run under this policy, and why not.
//
// The deny half stays with the permission gate, which already evaluates deny
// rules ahead of every permissive path; this covers the allow-list, which has
// no equivalent there.
func (p Policy) ToolAllowed(tool string) (bool, string) {
	if len(p.Tools.AllowOnly) == 0 {
		return true, ""
	}
	for _, pattern := range p.Tools.AllowOnly {
		if globMatch(pattern, tool) {
			return true, ""
		}
	}
	return false, "policy allows only " + strings.Join(p.Tools.AllowOnly, ", ")
}

// ModelAllowed reports whether a model may be used, and why not.
//
// providerID identifies the provider so the local flag and the price list can
// be read from the catalogue; an unknown provider or model is not refused,
// because a custom OpenAI-compatible endpoint is legitimate and has no entry.
// RequireLocal is the exception: it refuses what it cannot confirm is local,
// since "unknown" is not an acceptable answer to "does this leave the machine".
func (p Policy) ModelAllowed(providerID, model string) (bool, string) {
	qualified := model
	if providerID != "" {
		qualified = providerID + "/" + model
	}

	for _, pattern := range p.Models.Deny {
		if globMatch(pattern, model) || globMatch(pattern, qualified) {
			return false, "policy denies " + pattern
		}
	}
	if len(p.Models.Allow) > 0 {
		var ok bool
		for _, pattern := range p.Models.Allow {
			if globMatch(pattern, model) || globMatch(pattern, qualified) {
				ok = true
				break
			}
		}
		if !ok {
			return false, "policy allows only " + strings.Join(p.Models.Allow, ", ")
		}
	}

	prov, known := LookupProvider(providerID)
	if p.Models.RequireLocal {
		if !known {
			return false, "policy requires a local model and " + providerID + " is not a known provider"
		}
		if !prov.Local {
			return false, "policy requires a local model; " + providerID + " is a hosted provider"
		}
	}

	if p.Models.MaxInputPrice > 0 || p.Models.MaxOutputPrice > 0 {
		m, found := LookupModel(providerID, model)
		if !found {
			return true, "" // unpriced: see the doc comment
		}
		if max := p.Models.MaxInputPrice; max > 0 && m.InputPrice > max {
			return false, fmt.Sprintf("input price $%.2f/Mtok is above the policy ceiling of $%.2f",
				m.InputPrice, max)
		}
		if max := p.Models.MaxOutputPrice; max > 0 && m.OutputPrice > max {
			return false, fmt.Sprintf("output price $%.2f/Mtok is above the policy ceiling of $%.2f",
				m.OutputPrice, max)
		}
	}
	return true, ""
}

// globMatch matches a name against a policy pattern. A trailing "*" is a prefix
// match and "*" alone matches everything; anything else is exact.
//
// Deliberately not a full glob. A policy is read by someone deciding whether it
// is correct, and an expressive pattern language makes that judgement harder
// rather than easier.
func globMatch(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == name
	}
}
