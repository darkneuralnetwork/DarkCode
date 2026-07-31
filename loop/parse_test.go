package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseUntil(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantCriterion string
		wantTask      string
		wantOK        bool
	}{
		{"backticked command", "until `go test ./...` passes: add retry logic",
			"go test ./...", "add retry logic", true},
		{"backticked without passes", "until `npm run lint`: tidy the code",
			"npm run lint", "tidy the code", true},
		{"bare file", "until src/index.html exists: build the landing page",
			"src/index.html exists", "build the landing page", true},
		{"case insensitive", "UNTIL `make` passes: fix the build",
			"make", "fix the build", true},

		{"no until prefix", "add retry logic", "", "", false},
		{"until with no separator", "until the tests pass", "", "", false},
		{"empty task", "until `go build`: ", "", "", false},
		{"empty criterion", "until : do the thing", "", "", false},
		// "until" inside ordinary prose must not be mistaken for the keyword.
		{"until mid-sentence", "keep retrying until it works", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, task, ok := ParseUntil(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (criterion=%q task=%q)", ok, tc.wantOK, c, task)
			}
			if !ok {
				return
			}
			if c != tc.wantCriterion {
				t.Errorf("criterion = %q, want %q", c, tc.wantCriterion)
			}
			if task != tc.wantTask {
				t.Errorf("task = %q, want %q", task, tc.wantTask)
			}
		})
	}
}

func TestFileCriterion(t *testing.T) {
	tests := []struct {
		in       string
		wantPath string
		wantOK   bool
	}{
		{"src/index.html exists", "src/index.html", true},
		{"src/index.html", "src/index.html", true},
		{"main.go", "main.go", true},
		{"go test ./...", "", false},
		{"npm run build", "", false},
		{"cat a | grep b", "", false},
	}
	for _, tc := range tests {
		p, ok := FileCriterion(tc.in)
		if ok != tc.wantOK || p != tc.wantPath {
			t.Errorf("FileCriterion(%q) = (%q,%v), want (%q,%v)", tc.in, p, ok, tc.wantPath, tc.wantOK)
		}
	}
}

// TestContractFromUntilChecksAFileForReal — an artifact criterion is answered
// by looking at the filesystem, not by running it as a command.
func TestContractFromUntilChecksAFileForReal(t *testing.T) {
	ws := t.TempDir()
	c := ContractFromUntil("out/page.html exists", ws, nil)

	if v := c.Verify(context.Background()); v.Passed {
		t.Error("verified before the file existed")
	}

	path := filepath.Join(ws, "out/page.html")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	v := c.Verify(context.Background())
	if !v.Passed || !v.Proven() {
		t.Errorf("did not verify after the file was written: %+v", v)
	}
}

// TestContractFromUntilRejectsAnEmptyArtifact. A file that exists but is empty
// is the shape of a task that "finished" without doing anything.
func TestContractFromUntilRejectsAnEmptyArtifact(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "page.html"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := ContractFromUntil("page.html exists", ws, nil)
	if v := c.Verify(context.Background()); v.Passed {
		t.Error("an empty file counted as the artifact being produced")
	}
}

// TestContractFromUntilRunsCommands and carries the real output back, since
// that output is the correction signal the loop feeds to the model.
func TestContractFromUntilRunsCommands(t *testing.T) {
	calls := 0
	c := ContractFromUntil("go test ./...", t.TempDir(), func(cmd string) (bool, string) {
		calls++
		if calls == 1 {
			return false, "FAIL\tpkg/thing\t0.01s\nundefined: helper"
		}
		return true, "ok"
	})

	v := c.Verify(context.Background())
	if v.Passed {
		t.Fatal("first check should have failed")
	}
	if !contains(v.Evidence, "undefined: helper") {
		t.Errorf("evidence lost the real output: %q", v.Evidence)
	}

	if v := c.Verify(context.Background()); !v.Proven() {
		t.Error("second check should have passed")
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
