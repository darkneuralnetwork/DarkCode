package permission

import "strings"

// ============================================================================
// DENY RULES
//
// User-authored rules that refuse an action outright. They are evaluated
// before every permissive path — before the relaxed level, before a
// session-wide "allow for session", before the approver is ever consulted —
// so "never let anything run `git push`" actually holds no matter how the
// session was configured or what was approved earlier.
//
// A rule is either a bare tool name ("browser") or "tool:pattern", where the
// pattern is matched against each string argument of the call with * and ?
// wildcards. Examples:
//
//	terminal:*rm -rf /*     refuse that command shape
//	write_file:*/.ssh/*     refuse writes anywhere under a .ssh directory
//	git:push                refuse pushing
//	browser                 refuse the tool entirely
// ============================================================================

// DenyRule is one parsed rule.
type DenyRule struct {
	Tool    string
	Pattern string // "" = the whole tool is denied
}

// ParseDenyRules turns the configured strings into rules, ignoring blanks.
func ParseDenyRules(rules []string) []DenyRule {
	var out []DenyRule
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		tool, pattern, _ := strings.Cut(r, ":")
		out = append(out, DenyRule{Tool: strings.TrimSpace(tool), Pattern: strings.TrimSpace(pattern)})
	}
	return out
}

// matches reports whether the rule refuses this call.
func (d DenyRule) matches(tool string, args map[string]interface{}) bool {
	if d.Tool != tool && d.Tool != "*" {
		return false
	}
	if d.Pattern == "" {
		return true
	}
	for _, v := range args {
		if s, ok := v.(string); ok && wildcardMatch(d.Pattern, s) {
			return true
		}
	}
	return false
}

// wildcardMatch reports whether s matches a glob of literals, '*' (any run,
// including separators) and '?' (any single character). Unlike filepath.Match
// a '*' deliberately spans '/' — these patterns describe commands and paths
// alike, and "*/.ssh/*" must reach an arbitrarily deep path.
func wildcardMatch(pattern, s string) bool {
	var pi, si, starP, starS int
	starP = -1
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			starP, starS = pi, si
			pi++
		case starP >= 0:
			// Backtrack: let the last '*' consume one more character.
			pi, starS = starP+1, starS+1
			si = starS
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
