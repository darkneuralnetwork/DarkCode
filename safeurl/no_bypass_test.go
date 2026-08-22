package safeurl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNothingBypassesTheGuardedDialer scans the tree for HTTP clients built
// outside this package.
//
// Air gap and the SSRF checks live in the dialer's Control hook, so they only
// apply to clients this package hands out. A raw http.Client or
// http.DefaultClient is a hole by construction — and there were nine of them,
// including the model downloader and the provider ping, so "refuse every
// connection that leaves the machine" was not true.
//
// A source scan rather than a runtime check because the hole is the existence
// of the client, not any particular call.
func TestNothingBypassesTheGuardedDialer(t *testing.T) {
	root := ".."

	// Patterns that construct or use an unguarded client.
	bad := []string{
		"http.DefaultClient",
		"&http.Client{",
		"http.Get(",
		"http.Post(",
		"http.Head(",
		"http.PostForm(",
	}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable paths are not our problem here
		}
		if info.IsDir() {
			switch info.Name() {
			// .claude holds nested agent git worktrees — full checkouts of other
			// branches. Walking them reports every branch's clients as offenders
			// in this branch's tree, so a stale worktree fails the check with no
			// code change here. Skip it like the other non-source trees.
			case ".git", ".claude", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// This package is where the guarded clients are built.
		if strings.HasPrefix(filepath.ToSlash(path), "../safeurl/") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // a comment mentioning the pattern is not a client
			}
			for _, p := range bad {
				if strings.Contains(line, p) {
					offenders = append(offenders,
						filepath.ToSlash(path)+":"+itoa(i+1)+": "+trimmed)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("%d HTTP client(s) bypass the guarded dialer, so air gap and the "+
			"SSRF checks do not apply to them. Use safeurl.EgressClient for a "+
			"user-configured endpoint, or safeurl.SafeClient for a URL a model "+
			"or web page chose:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
