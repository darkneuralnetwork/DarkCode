package selfheal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/kernel/candidate"
)

// gitRepo builds a real repository, because Stage's whole job is git
// interaction and a fake would test the fake.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"add", "."},
		{"commit", "-qm", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func healer(t *testing.T, dir, verify string, gen GenerateFunc) *Healer {
	t.Helper()
	return &Healer{Workspace: dir, Verify: verify, Generate: gen, MaxFixes: 5}
}

// The gate. A fix that does not pass the verifier must not come out at all.
func TestUnverifiedFixesAreNotProposed(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "original"})

	h := healer(t, dir, "false", // the verifier always fails
		func(_ context.Context, i Issue) ([]candidate.Patch, error) {
			return []candidate.Patch{
				{ID: "attempt-1", Files: map[string]string{"a.txt": "one"}},
				{ID: "attempt-2", Files: map[string]string{"a.txt": "two"}},
			}, nil
		})

	fixes, err := h.Propose(context.Background(), []Issue{{Kind: "missing-test", Subject: "a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 0 {
		t.Errorf("a fix that failed the verifier was proposed anyway: %+v", fixes)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(body) != "original" {
		t.Errorf("the workspace was left modified: %q", body)
	}
}

// A fix that passes is proposed, and carries the evidence.
func TestVerifiedFixIsProposedWithEvidence(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "original"})

	h := healer(t, dir, "grep -q fixed a.txt",
		func(_ context.Context, i Issue) ([]candidate.Patch, error) {
			return []candidate.Patch{
				{ID: "wrong", Source: "model-a", Files: map[string]string{"a.txt": "still broken"}},
				{ID: "right", Source: "model-b", Files: map[string]string{"a.txt": "fixed"}},
			}, nil
		})

	fixes, err := h.Propose(context.Background(),
		[]Issue{{Kind: "missing-test", Subject: "a.txt", Detail: "a.txt has no test"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 1 {
		t.Fatalf("got %d fixes, want the one that verified", len(fixes))
	}
	f := fixes[0]
	if f.Patch.ID != "right" {
		t.Errorf("kept %q, want the candidate that passed", f.Patch.ID)
	}
	if !strings.Contains(f.Body, "grep -q fixed a.txt") {
		t.Errorf("the body does not say what verified it:\n%s", f.Body)
	}
	if len(f.Rejected) == 0 {
		t.Error("the rejected candidate was not recorded; a reviewer should see what else was tried")
	}
	if !strings.Contains(f.Body, "run, not reviewed") {
		t.Errorf("the body should not overstate what was checked:\n%s", f.Body)
	}
	if !strings.HasPrefix(f.Branch, "selfheal/") {
		t.Errorf("branch = %q, want a namespaced branch", f.Branch)
	}
}

func TestProposeNeedsAGeneratorAndAVerifier(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})

	if _, err := (&Healer{Workspace: dir}).Propose(context.Background(), []Issue{{}}); err == nil {
		t.Error("proposing without a generator should be refused")
	}

	// A directory with no recognisable build file has no default verifier.
	bare := t.TempDir()
	h := &Healer{Workspace: bare, Generate: func(context.Context, Issue) ([]candidate.Patch, error) {
		return []candidate.Patch{{ID: "x", Files: map[string]string{"a": "b"}}}, nil
	}}
	if _, err := h.Propose(context.Background(), []Issue{{}}); err == nil {
		t.Error("proposing with no way to verify should be refused, not attempted")
	}
}

// MaxFixes is about the person reviewing, so it must actually bound the run.
func TestProposeStopsAtMaxFixes(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "x"})
	h := healer(t, dir, "true", func(_ context.Context, i Issue) ([]candidate.Patch, error) {
		return []candidate.Patch{{ID: "p-" + i.Subject, Files: map[string]string{i.Subject: "new"}}}, nil
	})
	h.MaxFixes = 2

	issues := []Issue{{Subject: "a.txt"}, {Subject: "b.txt"}, {Subject: "c.txt"}, {Subject: "d.txt"}}
	fixes, err := h.Propose(context.Background(), issues)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 2 {
		t.Errorf("got %d fixes, want the configured limit of 2", len(fixes))
	}
}

// A generator that fails for one issue must not sink the whole run.
func TestGeneratorFailureSkipsOnlyThatIssue(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "x", "b.txt": "y"})
	h := healer(t, dir, "true", func(_ context.Context, i Issue) ([]candidate.Patch, error) {
		if i.Subject == "a.txt" {
			return nil, os.ErrNotExist
		}
		return []candidate.Patch{{ID: "ok", Files: map[string]string{"b.txt": "new"}}}, nil
	})
	fixes, err := h.Propose(context.Background(), []Issue{{Subject: "a.txt"}, {Subject: "b.txt"}})
	if err != nil {
		t.Fatalf("one bad generator call failed the run: %v", err)
	}
	if len(fixes) != 1 {
		t.Errorf("got %d fixes, want the one that could be generated", len(fixes))
	}
}

// --- Stage ---

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestStageCommitsToItsOwnBranchAndReturns(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "original"})
	h := healer(t, dir, "true", nil)

	fix := Fix{
		Issue:  Issue{Kind: "missing-test", Subject: "a.txt"},
		Patch:  candidate.Patch{ID: "p", Files: map[string]string{"a.txt": "fixed"}},
		Branch: "selfheal/missing-test-a-txt", Title: "add a test for a.txt", Body: "because",
	}
	if err := h.Stage(context.Background(), fix); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Back on the original branch, with the original content.
	if got := currentBranch(t, dir); got != "main" {
		t.Errorf("left on branch %q, want main", got)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(body) != "original" {
		t.Errorf("main's copy of a.txt was modified: %q", body)
	}

	// The work is on the branch.
	cmd := exec.Command("git", "show", fix.Branch+":a.txt")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the branch does not carry the fix: %v", err)
	}
	if strings.TrimSpace(string(out)) != "fixed" {
		t.Errorf("branch content = %q, want the fix", out)
	}
}

// Staging on top of uncommitted work would mix the two.
func TestStageRefusesADirtyTree(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "original"})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("user edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := healer(t, dir, "true", nil)

	err := h.Stage(context.Background(), Fix{
		Patch:  candidate.Patch{ID: "p", Files: map[string]string{"a.txt": "fix"}},
		Branch: "selfheal/x", Title: "t",
	})
	if err == nil {
		t.Fatal("Stage ran on a dirty tree")
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(body) != "user edit" {
		t.Errorf("the user's uncommitted edit was disturbed: %q", body)
	}
}

// --- naming ---

func TestBranchNamesAreSafeAndStable(t *testing.T) {
	for _, tc := range []struct{ subject, want string }{
		{"pkg/Foo.go", "selfheal/missing-test-pkg-foo-go"},
		{"Weird  Name!!", "selfheal/missing-test-weird-name"},
		{"", "selfheal/missing-test-issue"},
	} {
		got := branchName(Issue{Kind: "missing-test", Subject: tc.subject})
		if got != tc.want {
			t.Errorf("branchName(%q) = %q, want %q", tc.subject, got, tc.want)
		}
		if strings.ContainsAny(got, " ~^:?*[\\") {
			t.Errorf("branch %q contains a character git refuses", got)
		}
	}
	long := branchName(Issue{Kind: "missing-test", Subject: strings.Repeat("abcdefghij", 12)})
	if len(long) > 70 {
		t.Errorf("branch name is %d chars, unreasonably long: %q", len(long), long)
	}
}

func TestFilesFromProvenance(t *testing.T) {
	for in, want := range map[string]string{
		"3 referencing file(s), none of them tests (pkg/foo.go:42)": "pkg/foo.go",
		"no other indexed file references it (a/b.go:1)":            "a/b.go",
		"no parenthesis here": "",
	} {
		got := filesFromProvenance(in)
		if want == "" {
			if len(got) != 0 {
				t.Errorf("filesFromProvenance(%q) = %v, want none", in, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("filesFromProvenance(%q) = %v, want [%s]", in, got, want)
		}
	}
}
