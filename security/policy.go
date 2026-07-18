package security

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Policy defines the declarative security rules for a project. It is persisted
// as JSON (a security.json at the project root); a missing file yields the
// safe default returned by DefaultPolicy.
type Policy struct {
	AllowedPaths []string `json:"allowed_paths"`
	DeniedPaths  []string `json:"denied_paths"`
	NetworkMode  string   `json:"network_mode"` // "none", "restricted", "full"
}

// DefaultPolicy is applied when no policy file is present: allow everything
// under the project (callers scope AllowedPaths to the workspace), deny the
// obvious credential stores, and restrict outbound network by default.
func DefaultPolicy() *Policy {
	return &Policy{
		AllowedPaths: []string{"*"},
		DeniedPaths: []string{
			"**/.ssh/**",
			"**/.aws/**",
			"**/.git/config",
			"**/.env",
		},
		NetworkMode: "restricted",
	}
}

// LoadPolicy reads a JSON security policy from path. A non-existent file is not
// an error — it returns DefaultPolicy so callers always get a usable policy.
func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultPolicy(), nil
		}
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	// Fill in defaults for any omitted section so a partial policy is still safe.
	if len(p.AllowedPaths) == 0 {
		p.AllowedPaths = []string{"*"}
	}
	if p.NetworkMode == "" {
		p.NetworkMode = "restricted"
	}
	return &p, nil
}

// PathAllowed reports whether path may be accessed under this policy. A denied
// glob always wins over an allowed one (deny is a hard override). Globs use
// filepath.Match semantics with support for a leading "**/" to mean "at any
// depth". An empty policy (no allowed paths) denies everything.
func (p *Policy) PathAllowed(path string) bool {
	clean := filepath.Clean(path)
	for _, pat := range p.DeniedPaths {
		if globMatch(pat, clean) {
			return false
		}
	}
	for _, pat := range p.AllowedPaths {
		if pat == "*" || globMatch(pat, clean) {
			return true
		}
	}
	return false
}

// globMatch matches pat against path, treating a leading "**/" as "match this
// tail at any directory depth" (filepath.Match itself does not cross separators
// for "*"). It also matches when the "**/"-stripped tail matches the basename
// or any suffix of the path.
func globMatch(pat, path string) bool {
	if strings.HasPrefix(pat, "**/") {
		tail := pat[3:]
		// Try the tail against the full path and against every suffix segment.
		if ok, _ := filepath.Match(tail, path); ok {
			return true
		}
		parts := strings.Split(path, string(filepath.Separator))
		for i := range parts {
			sub := strings.Join(parts[i:], string(filepath.Separator))
			if ok, _ := filepath.Match(tail, sub); ok {
				return true
			}
		}
		return false
	}
	ok, _ := filepath.Match(pat, path)
	return ok
}
