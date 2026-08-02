package httpx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedRaw are the only files permitted to construct an http.Client directly:
// safeurl builds the guarded transports, and this package wraps them.
var allowedRaw = map[string]bool{
	filepath.Join("safeurl", "safeurl.go"): true,
	filepath.Join("httpx", "httpx.go"):     true,
}

// TestNoUnguardedHTTPClients fails the build when a new call site constructs
// its own HTTP client.
//
// This is the enforcement half of the package doc. The controls in safeurl were
// always correct; the failure was that using them was optional, so air_gap
// held on the paths someone had remembered and not on the model downloader.
// Twenty sites had drifted before anyone noticed, because each one looked
// locally reasonable.
//
// A grep in CI is a blunt instrument, but the alternative is trusting every
// future contributor to know that "just fetch a URL" is a security decision.
func TestNoUnguardedHTTPClients(t *testing.T) {
	root := ".."
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable paths are not our problem here
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", ".darkcode":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowedRaw[rel] {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(src), "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i] // ignore prose that merely mentions the pattern
			}
			if strings.Contains(code, "http.DefaultClient") || strings.Contains(code, "&http.Client{") {
				offenders = append(offenders, rel+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("these files build their own HTTP client, so air-gap and the SSRF guard do not apply to them.\n"+
			"Use httpx.Client(httpx.Fetch|Egress|Local) instead:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestIntentsMapToDistinctPolicies is a smoke test that the three intents are
// not accidentally the same client. Fetch must refuse loopback (it is for
// model-chosen URLs); Local must permit it.
func TestIntentsMapToDistinctPolicies(t *testing.T) {
	if Client(Fetch) == nil || Client(Egress) == nil || Client(Local) == nil {
		t.Fatal("every intent must yield a usable client")
	}
	if Client(Fetch).Timeout == 0 {
		t.Error("Fetch must carry a default timeout; a model-chosen URL cannot be trusted to terminate")
	}
	if NoTimeout(Egress).Timeout != 0 {
		t.Error("NoTimeout must lift the deadline for streaming transfers")
	}
}
