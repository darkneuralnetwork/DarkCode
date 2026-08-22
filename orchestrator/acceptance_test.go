package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcceptanceCommandExtraction(t *testing.T) {
	cases := map[string]string{
		"go test ./...":                        "go test ./...",
		"Run `npm run lint` and it passes":     "npm run lint",
		"  make check  ":                       "make check",
		"./scripts/verify.sh":                  "./scripts/verify.sh",
		"pytest -q":                            "pytest -q",
		"The API returns 200 for valid input":  "",
		"Users can log in":                     "",
		"Documentation is updated":             "",
		"cargo test --all":                     "cargo test --all",
		"golangci-lint run":                    "golangci-lint run",
		"Code review confirms the design fits": "",
	}
	for criterion, want := range cases {
		if got := acceptanceCommand(criterion); got != want {
			t.Errorf("acceptanceCommand(%q) = %q, want %q", criterion, got, want)
		}
	}
}

// A backticked command wins over a prose-looking prefix, since it is the
// author being explicit about what to run.
func TestAcceptanceCommandPrefersBackticks(t *testing.T) {
	got := acceptanceCommand("The build succeeds: `go build ./...`")
	if got != "go build ./..." {
		t.Errorf("got %q, want the backticked command", got)
	}
}

func TestDefaultAcceptancePerProjectType(t *testing.T) {
	cases := map[string]string{
		"go.mod":         "go build ./... && go test ./...",
		"package.json":   "npm test --silent",
		"Cargo.toml":     "cargo test",
		"pyproject.toml": "pytest -q",
	}
	for marker, want := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := defaultAcceptance(dir); got != want {
			t.Errorf("%s project → %q, want %q", marker, got, want)
		}
	}

	// An unrecognised project gets no fabricated command.
	if got := defaultAcceptance(t.TempDir()); got != "" {
		t.Errorf("unknown project type → %q, want no default", got)
	}
}
