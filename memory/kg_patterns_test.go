package memory

import (
	"testing"

	"github.com/darkcode/core"
)

// patternGraph builds a repository with a deliberate convention and a
// deliberate exception to it.
func patternGraph(t *testing.T, files ...string) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)
	for _, f := range files {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + f, Label: f, Type: core.KGNodeFile,
			Confidence: 1, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	return kg
}

// A convention with one exception is the case worth reporting.
func TestMineFindsTestCompanionConvention(t *testing.T) {
	kg := patternGraph(t,
		"svc/a.go", "svc/a_test.go",
		"svc/b.go", "svc/b_test.go",
		"svc/c.go", "svc/c_test.go",
		"svc/d.go", "svc/d_test.go",
		"svc/e.go", "svc/e_test.go",
		"svc/lonely.go", // the exception
	)
	patterns := kg.MinePatterns("repo-a")

	var found *Pattern
	for i := range patterns {
		if patterns[i].Kind == "test-companion" && patterns[i].Subject == "svc" {
			found = &patterns[i]
		}
	}
	if found == nil {
		t.Fatalf("the test convention was not mined: %+v", patterns)
	}
	if found.Support != 6 || found.Holds != 5 {
		t.Errorf("support/holds = %d/%d, want 6/5", found.Support, found.Holds)
	}
	if found.Origin != "repo-a" {
		t.Errorf("origin = %q, want the repository it was mined from", found.Origin)
	}

	violations := kg.CheckPatterns(patterns)
	var named bool
	for _, v := range violations {
		if v.File == "svc/lonely.go" {
			named = true
		}
	}
	if !named {
		t.Errorf("the untested file was not reported: %+v", violations)
	}
}

// Three files that happen to agree are not a convention.
func TestMineIgnoresWeakSupport(t *testing.T) {
	kg := patternGraph(t, "tiny/a.go", "tiny/a_test.go", "tiny/b.go", "tiny/b_test.go")
	for _, p := range kg.MinePatterns("repo-a") {
		if p.Kind == "test-companion" && p.Subject == "tiny" {
			t.Errorf("a %d-case coincidence was mined as a convention: %+v", p.Support, p)
		}
	}
}

// A package that mostly does not test is not a testing convention.
func TestMineIgnoresLowConfidence(t *testing.T) {
	kg := patternGraph(t,
		"loose/a.go", "loose/a_test.go",
		"loose/b.go", "loose/c.go", "loose/d.go", "loose/e.go", "loose/f.go",
	)
	for _, p := range kg.MinePatterns("repo-a") {
		if p.Kind == "test-companion" && p.Subject == "loose" {
			t.Errorf("a %.0f%% rule was mined as a convention", p.Confidence()*100)
		}
	}
}

func TestTestSubjectMapsBackToTheCoveredFile(t *testing.T) {
	for in, want := range map[string]string{
		"pkg/foo_test.go": "pkg/foo.go",
		"pkg/test_foo.py": "pkg/foo.py",
		"src/foo.test.ts": "src/foo.ts",
		"src/foo.spec.js": "src/foo.js",
		"pkg/notatest.go": "pkg/notatest.go",
	} {
		if got := testSubject(in); got != want {
			t.Errorf("testSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// The cross-repository case: a rule learned in one repo, applied to another.
func TestPatternsTransferBetweenRepositories(t *testing.T) {
	// Repo A keeps its layering: store never imports http.
	repoA := patternGraph(t,
		"store/a.go", "store/b.go", "store/c.go", "store/d.go", "store/e.go",
		"http/h.go", "core/c.go")
	addImport(t, repoA, "http/h.go", "example.com/m/store")
	addImport(t, repoA, "http/h.go", "example.com/m/core")

	learned := repoA.MinePatterns("repo-a")
	var boundary bool
	for _, p := range learned {
		if p.Kind == "layering" && p.Subject == "store" && p.Expect == "http" {
			boundary = true
		}
	}
	if !boundary {
		t.Fatalf("the store↛http boundary was not mined: %+v", learned)
	}

	// Repo B breaks it.
	repoB := patternGraph(t,
		"store/a.go", "store/b.go", "store/c.go", "store/d.go", "store/e.go",
		"http/h.go", "core/c.go")
	addImport(t, repoB, "store/a.go", "example.com/m/http") // the violation
	addImport(t, repoB, "http/h.go", "example.com/m/core")

	violations := repoB.CheckPatterns(learned)
	var caught bool
	for _, v := range violations {
		if v.Pattern.Kind == "layering" && v.File == "store" {
			caught = true
			if v.Pattern.Origin != "repo-a" {
				t.Errorf("the violation should attribute repo-a, got %q", v.Pattern.Origin)
			}
		}
	}
	if !caught {
		t.Errorf("the imported-across-the-boundary violation was missed: %+v", violations)
	}
}

// Mining twice must produce the same library, or nothing can be compared
// between commits.
func TestMineIsDeterministic(t *testing.T) {
	kg := patternGraph(t,
		"a/1.go", "a/2.go", "a/3.go", "a/4.go", "a/5.go",
		"b/1.go", "b/2.go", "b/3.go", "b/4.go", "b/5.go",
		"c/1.go", "c/2.go", "c/3.go", "c/4.go", "c/5.go",
		"d/1.go")
	addImport(t, kg, "a/1.go", "example.com/m/d")
	addImport(t, kg, "b/1.go", "example.com/m/d")
	addImport(t, kg, "c/1.go", "example.com/m/d")

	first := kg.MinePatterns("r")
	for i := 0; i < 15; i++ {
		got := kg.MinePatterns("r")
		if len(got) != len(first) {
			t.Fatalf("mined %d patterns, previously %d", len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("pattern %d differs between runs:\n%+v\n%+v", j, got[j], first[j])
			}
		}
	}
}

// --- library ---

func TestLibraryKeepsRepositoriesSeparate(t *testing.T) {
	dir := t.TempDir()
	lib := NewPatternLibrary(dir)

	lib.Learn("repo-a", []Pattern{{Kind: "layering", Subject: "store", Expect: "http", Support: 9, Holds: 9, Origin: "repo-a"}})
	lib.Learn("repo-b", []Pattern{{Kind: "test-companion", Subject: "svc", Support: 8, Holds: 8, Origin: "repo-b"}})

	if got := len(lib.All()); got != 2 {
		t.Errorf("library holds %d patterns, want 2", got)
	}
	if got := lib.Elsewhere("repo-a"); len(got) != 1 || got[0].Origin != "repo-b" {
		t.Errorf("Elsewhere(repo-a) = %+v, want only repo-b's rules", got)
	}

	// Re-mining a repository replaces its contribution instead of doubling it.
	lib.Learn("repo-a", []Pattern{{Kind: "layering", Subject: "store", Expect: "ui", Support: 9, Holds: 9, Origin: "repo-a"}})
	if got := len(lib.All()); got != 2 {
		t.Errorf("re-learning duplicated rules: %d, want 2", got)
	}
}

func TestLibraryPersists(t *testing.T) {
	dir := t.TempDir()
	first := NewPatternLibrary(dir)
	first.Learn("repo-a", []Pattern{{Kind: "test-companion", Subject: "svc", Support: 7, Holds: 7}})

	second := NewPatternLibrary(dir)
	if got := len(second.All()); got != 1 {
		t.Errorf("after reopening the library holds %d patterns, want 1", got)
	}
	if repos := second.Repos(); len(repos) != 1 || repos[0] != "repo-a" {
		t.Errorf("Repos() = %v, want [repo-a]", repos)
	}
}

func TestLibraryWithoutADirectoryStaysInMemory(t *testing.T) {
	lib := NewPatternLibrary("")
	lib.Learn("r", []Pattern{{Kind: "layering", Subject: "a", Expect: "b"}})
	if got := len(lib.All()); got != 1 {
		t.Errorf("in-memory library lost its patterns: %d", got)
	}
}
