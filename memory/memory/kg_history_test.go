package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// eventKinds indexes a diff by kind → subjects.
func eventKinds(events []Event) map[string][]string {
	out := map[string][]string{}
	for _, e := range events {
		out[e.Kind] = append(out[e.Kind], e.Subject)
	}
	return out
}

func snap(files map[string][]string, imports map[string][]string) Snapshot {
	return Snapshot{Symbols: files, Imports: imports}
}

func TestDiffDetectsFileLifecycle(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"Alpha"}, "gone.go": {"Old"}}, nil)
	after := snap(map[string][]string{"a.go": {"Alpha"}, "new.go": {"Fresh"}}, nil)

	kinds := eventKinds(diffSnapshots(before, after))
	if got := kinds["NewComponent"]; len(got) != 1 || got[0] != "new.go" {
		t.Errorf("NewComponent = %v, want [new.go]", got)
	}
	if got := kinds["ComponentRemoved"]; len(got) != 1 || got[0] != "gone.go" {
		t.Errorf("ComponentRemoved = %v, want [gone.go]", got)
	}
}

func TestDiffDetectsAPIBreak(t *testing.T) {
	before := snap(map[string][]string{"api.go": {"Exported", "unexported"}}, nil)
	after := snap(map[string][]string{"api.go": {"unexported"}}, nil)

	kinds := eventKinds(diffSnapshots(before, after))
	if len(kinds["APIBroken"]) != 1 {
		t.Fatalf("expected an APIBroken event, got %v", kinds)
	}

	// Removing an unexported symbol is not a break.
	before2 := snap(map[string][]string{"api.go": {"Exported", "unexported"}}, nil)
	after2 := snap(map[string][]string{"api.go": {"Exported"}}, nil)
	if k := eventKinds(diffSnapshots(before2, after2)); len(k["APIBroken"]) != 0 {
		t.Errorf("removing an unexported symbol reported as a break: %v", k)
	}
}

// Renaming a test function must not read as a breaking change.
func TestDiffIgnoresTestFileSymbols(t *testing.T) {
	before := snap(map[string][]string{"pkg/thing_test.go": {"TestOldName"}}, nil)
	after := snap(map[string][]string{"pkg/thing_test.go": {"TestNewName"}}, nil)

	if k := eventKinds(diffSnapshots(before, after)); len(k["APIBroken"]) != 0 {
		t.Errorf("test rename reported as an API break: %v", k)
	}
}

// Language rules differ: Go uses capitalisation, the rest use a leading
// underscore.
func TestIsExportedIsLanguageAware(t *testing.T) {
	cases := []struct {
		file, name string
		want       bool
	}{
		{"a.go", "Exported", true},
		{"a.go", "unexported", false},
		{"a.py", "public_fn", true},
		{"a.py", "_private", false},
		{"a.ts", "handler", true},
		{"a.ts", "_internal", false},
		{"a.rs", "run", true},
	}
	for _, c := range cases {
		if got := isExported(c.file, c.name); got != c.want {
			t.Errorf("isExported(%q, %q) = %v, want %v", c.file, c.name, got, c.want)
		}
	}
}

func TestDiffDetectsDependencyChanges(t *testing.T) {
	before := snap(nil, map[string][]string{"api": {"core"}})
	after := snap(nil, map[string][]string{"api": {"core", "billing"}, "billing": {"core"}})

	kinds := eventKinds(diffSnapshots(before, after))
	if len(kinds["DependencyIntroduced"]) != 2 {
		t.Errorf("DependencyIntroduced = %v, want api→billing and billing→core", kinds["DependencyIntroduced"])
	}

	// And the reverse direction.
	back := eventKinds(diffSnapshots(after, before))
	if len(back["DependencyRemoved"]) != 2 {
		t.Errorf("DependencyRemoved = %v", back["DependencyRemoved"])
	}
}

// The headline event: a commit that closes a dependency loop.
func TestDiffDetectsCycleCreationAndResolution(t *testing.T) {
	acyclic := snap(nil, map[string][]string{"auth": {"core"}, "billing": {"core"}})
	cyclic := snap(nil, map[string][]string{"auth": {"billing"}, "billing": {"auth"}})

	created := eventKinds(diffSnapshots(acyclic, cyclic))
	if len(created["CycleCreated"]) == 0 {
		t.Fatalf("cycle creation not reported: %v", created)
	}
	resolved := eventKinds(diffSnapshots(cyclic, acyclic))
	if len(resolved["CycleResolved"]) == 0 {
		t.Errorf("cycle resolution not reported: %v", resolved)
	}

	// A cycle present in both must be reported in neither.
	if k := eventKinds(diffSnapshots(cyclic, cyclic)); len(k["CycleCreated"])+len(k["CycleResolved"]) != 0 {
		t.Errorf("unchanged cycle produced events: %v", k)
	}
}

func TestDiffRanksBySeverity(t *testing.T) {
	before := snap(map[string][]string{"api.go": {"Exported"}}, map[string][]string{"a": {"b"}})
	after := snap(map[string][]string{"api.go": {}, "new.go": {}}, map[string][]string{"a": {"b"}, "b": {"a"}})

	events := diffSnapshots(before, after)
	if len(events) < 2 {
		t.Fatal("expected several events")
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].Severity < events[i].Severity {
			t.Errorf("events not ranked by severity: %+v", events)
			break
		}
	}
	// A new cycle is the most serious thing that can happen structurally.
	if events[0].Kind != "CycleCreated" {
		t.Errorf("top event = %s, want CycleCreated", events[0].Kind)
	}
}

// SnapshotAt must read from git without disturbing the working tree.
func TestSnapshotAtDoesNotTouchWorkingTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package m\n\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")

	// Uncommitted edit that must survive and must not appear in the snapshot.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package m\n\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := SnapshotAt(dir, "HEAD")
	if err != nil {
		t.Fatalf("SnapshotAt: %v", err)
	}
	if syms := s.Symbols["a.go"]; len(syms) != 1 || syms[0] != "Alpha" {
		t.Errorf("snapshot = %v, want the committed Alpha, not the working-tree Beta", syms)
	}
	current, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil || string(current) != "package m\n\nfunc Beta() {}\n" {
		t.Errorf("working tree was modified: %q, %v", current, err)
	}
}
