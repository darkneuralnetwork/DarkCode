package memory

// kg_policy.go — architecture you can check (report #59, #92).
//
// Mined patterns (kg_patterns.go) describe what a repository *does*. A policy
// declares what it *should* do, and the two answer different questions. Mining
// tells you the storage layer currently avoids the HTTP layer; a policy says
// it must, so the day somebody adds the import it is a failure rather than a
// pattern that quietly stopped holding at 98%.
//
// That difference matters most across repositories, which is the governance
// case: one policy file, checked in, applied to every service, so an
// architectural decision survives the meeting it was made in.
//
// The rule vocabulary is small on purpose. Each rule maps to a question the
// graph can answer exactly — does this edge exist, is this file tested, is
// this number above that threshold — so a violation is a fact rather than an
// opinion, and there is nothing to argue with except the rule itself. A richer
// language would need a parser, an evaluator, and a way to explain what it
// concluded, which is more machinery than the decisions warrant.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/darkcode/infra/core"
)

// Rule is one architectural constraint.
type Rule struct {
	// Name is how the violation is reported. Required: "rule 3 failed" tells
	// nobody why anyone cared.
	Name string `json:"name"`
	// Kind is "forbid-import", "require-tests", "max-coupling" or "max-cycles".
	Kind string `json:"kind"`
	// From and To name packages for forbid-import. Both accept a trailing "*"
	// so a rule can cover a family of packages.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Package scopes require-tests; empty means the whole repository.
	Package string `json:"package,omitempty"`
	// Threshold is the ceiling for max-coupling and max-cycles, and the
	// minimum tested fraction (0..1) for require-tests.
	Threshold float64 `json:"threshold,omitempty"`
	// Why records the reasoning. A rule whose purpose is undocumented gets
	// deleted the first time it is inconvenient.
	Why string `json:"why,omitempty"`
}

// Breach is a rule that does not hold.
type Breach struct {
	Rule   Rule   `json:"rule"`
	Detail string `json:"detail"`
	// Subject is the file or package at fault, for grouping.
	Subject string `json:"subject,omitempty"`
}

// Policy is a set of rules, as loaded from a file.
type Policy struct {
	Rules []Rule `json:"rules"`
}

// LoadPolicy reads a policy file. A missing file is an empty policy, not an
// error: most repositories have no policy, and that is a valid state rather
// than a misconfiguration.
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
	for i, r := range p.Rules {
		if err := r.validate(); err != nil {
			return Policy{}, fmt.Errorf("%s: rule %d: %w", path, i+1, err)
		}
	}
	return p, nil
}

// validate rejects a rule that cannot mean anything, at load time rather than
// silently passing every check later.
func (r Rule) validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("needs a name")
	}
	switch r.Kind {
	case "forbid-import":
		if r.From == "" || r.To == "" {
			return fmt.Errorf("forbid-import needs from and to")
		}
	case "require-tests":
		if r.Threshold < 0 || r.Threshold > 1 {
			return fmt.Errorf("require-tests threshold is a fraction between 0 and 1, got %v", r.Threshold)
		}
	case "max-coupling", "max-cycles":
		if r.Threshold < 0 {
			return fmt.Errorf("%s threshold cannot be negative", r.Kind)
		}
	default:
		return fmt.Errorf("unknown kind %q (want forbid-import, require-tests, max-coupling, max-cycles)", r.Kind)
	}
	return nil
}

// CheckPolicy evaluates every rule against the graph and returns what fails.
//
// An empty result means the policy holds. Rules are evaluated independently,
// so one impossible rule does not hide the rest.
func (kg *KnowledgeGraph) CheckPolicy(p Policy) []Breach {
	var out []Breach
	graph := kg.packageGraph()

	for _, rule := range p.Rules {
		switch rule.Kind {
		case "forbid-import":
			out = append(out, kg.checkForbidImport(rule, graph)...)
		case "require-tests":
			out = append(out, kg.checkRequireTests(rule)...)
		case "max-coupling":
			m := measure(graph)
			if m.AvgCoupling > rule.Threshold {
				out = append(out, Breach{Rule: rule, Subject: "repository", Detail: fmt.Sprintf(
					"average coupling is %.2f, above the ceiling of %.2f", m.AvgCoupling, rule.Threshold)})
			}
		case "max-cycles":
			if n := len(FindCycles(graph)); float64(n) > rule.Threshold {
				out = append(out, Breach{Rule: rule, Subject: "repository", Detail: fmt.Sprintf(
					"%d import cycle(s), above the ceiling of %.0f", n, rule.Threshold)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rule.Name != out[j].Rule.Name {
			return out[i].Rule.Name < out[j].Rule.Name
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

func (kg *KnowledgeGraph) checkForbidImport(rule Rule, graph map[string][]string) []Breach {
	var out []Breach
	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs) // deterministic order out of map iteration

	for _, from := range pkgs {
		if !globMatch(rule.From, from) {
			continue
		}
		deps := append([]string(nil), graph[from]...)
		sort.Strings(deps)
		for _, to := range deps {
			if globMatch(rule.To, to) {
				out = append(out, Breach{Rule: rule, Subject: from,
					Detail: from + " imports " + to})
			}
		}
	}
	return out
}

func (kg *KnowledgeGraph) checkRequireTests(rule Rule) []Breach {
	threshold := rule.Threshold
	if threshold == 0 {
		threshold = 1 // an unstated requirement means "all of them"
	}

	tested := map[string]bool{}
	bySubject := map[string][]string{}
	for _, n := range kg.FindByType(core.KGNodeFile) {
		if isTestFile(n.Label) {
			tested[testSubject(n.Label)] = true
			continue
		}
		pkg := path.Dir(n.Label)
		if rule.Package != "" && !globMatch(rule.Package, pkg) {
			continue
		}
		bySubject[pkg] = append(bySubject[pkg], n.Label)
	}

	pkgs := make([]string, 0, len(bySubject))
	for p := range bySubject {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	var out []Breach
	for _, pkg := range pkgs {
		files := bySubject[pkg]
		var covered int
		for _, f := range files {
			if tested[f] {
				covered++
			}
		}
		got := float64(covered) / float64(len(files))
		if got+1e-9 < threshold {
			out = append(out, Breach{Rule: rule, Subject: pkg, Detail: fmt.Sprintf(
				"%d of %d files tested (%.0f%%), below the required %.0f%%",
				covered, len(files), got*100, threshold*100)})
		}
	}
	return out
}

// globMatch matches a package name against a rule pattern. A trailing "*" is
// a prefix match and "*" alone matches everything; anything else is exact.
// Deliberately not a full glob: a policy is read by people deciding whether it
// is fair, and an expressive pattern language makes that harder, not easier.
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

// FormatBreaches renders a policy result for a human, a model, or CI output.
func FormatBreaches(breaches []Breach) string {
	if len(breaches) == 0 {
		return "policy holds: no violations"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d policy violation(s)\n", len(breaches))

	var current string
	for _, x := range breaches {
		if x.Rule.Name != current {
			current = x.Rule.Name
			fmt.Fprintf(&b, "\n%s", current)
			if x.Rule.Why != "" {
				fmt.Fprintf(&b, " — %s", x.Rule.Why)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  · %s\n", x.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}
