package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
)

func TestLooksLikeFix(t *testing.T) {
	fixes := []string{
		"fix: nil pointer in handler", "Fix crash on empty input",
		"repair broken retry logic", "revert \"add caching\"",
		"resolve race in the scheduler", "hotfix for the deploy",
		"work around upstream bug",
	}
	for _, s := range fixes {
		if !looksLikeFix(s) {
			t.Errorf("looksLikeFix(%q) = false, want true", s)
		}
	}
	// Substring matching would misclassify all of these.
	features := []string{
		"add checkpoint support", "rewrite the README",
		"introduce the graph query tool", "bump go to 1.24",
		"add buggy feature", "improve debug logging",
		"extract the prefix helper", "add test fixtures",
		"refactor the debugger bridge",
	}
	for _, s := range features {
		if looksLikeFix(s) {
			t.Errorf("looksLikeFix(%q) = true, want false", s)
		}
	}
}

// gitRepo builds a repository with a known history shape.
func gitRepo(t *testing.T) string {
	t.Helper()
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
	run("config", "user.email", "a@example.com")
	run("config", "user.name", "Author A")

	commit := func(file, content, msg string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-qm", msg)
	}
	commit("buggy.go", "package p\n// v1\n", "add buggy feature")
	commit("buggy.go", "package p\n// v2\n", "fix crash in buggy")
	commit("buggy.go", "package p\n// v3\n", "fix another bug in buggy")
	commit("stable.go", "package p\n// s1\n", "add stable feature")
	return dir
}

func TestMineDefectHistorySeparatesFixesFromChurn(t *testing.T) {
	h, err := MineDefectHistory(gitRepo(t), 0)
	if err != nil {
		t.Fatalf("MineDefectHistory: %v", err)
	}
	if h.TotalFixes != 2 {
		t.Errorf("TotalFixes = %d, want 2", h.TotalFixes)
	}
	if h.Fixes["buggy.go"] != 2 {
		t.Errorf("buggy.go fixes = %d, want 2", h.Fixes["buggy.go"])
	}
	if h.Churn["buggy.go"] != 3 {
		t.Errorf("buggy.go churn = %d, want 3 (all commits)", h.Churn["buggy.go"])
	}
	if h.Fixes["stable.go"] != 0 {
		t.Errorf("stable.go should have no fixes, got %d", h.Fixes["stable.go"])
	}
	if h.Authors["buggy.go"] != 1 {
		t.Errorf("authors = %d, want 1", h.Authors["buggy.go"])
	}
	if h.LastFix["buggy.go"].IsZero() {
		t.Error("LastFix not recorded")
	}
}

// riskGraph gives two files, one referenced widely and one not.
func riskGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)

	for _, f := range []string{"hot.go", "cold.go", "caller1.go", "caller2.go"} {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + f, Label: f, Type: core.KGNodeFile,
			Confidence: 1, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(sym, file string, refs ...string) {
		id := "symbol:" + sym + "@" + file
		if err := kg.AddNode(&core.KGNode{ID: id, Label: sym, Type: core.KGNodeSymbol, Confidence: 1,
			Properties: map[string]string{"origin": "code_index", "kind": "function", "references": "0"}}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + file, To: id, Relation: core.KGRelDefines, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		for _, r := range refs {
			if err := kg.AddEdge(&core.KGEdge{From: "file:" + r, To: id, Relation: core.KGRelReferences, Weight: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("Hot", "hot.go", "caller1.go", "caller2.go")
	add("Cold", "cold.go")
	return kg
}

// Fan-in must count files that reference this one, not files it references.
func TestDefectRiskFanInIsInbound(t *testing.T) {
	kg := riskGraph(t)
	h := DefectHistory{
		Fixes: map[string]int{"hot.go": 1, "cold.go": 1, "caller1.go": 1},
		Churn: map[string]int{"hot.go": 2, "cold.go": 2, "caller1.go": 2},
	}
	byFile := map[string]Risk{}
	for _, r := range kg.DefectRisk(h, 0) {
		byFile[r.File] = r
	}
	if byFile["hot.go"].FanIn != 2 {
		t.Errorf("hot.go fan-in = %d, want 2 inbound references", byFile["hot.go"].FanIn)
	}
	if byFile["cold.go"].FanIn != 0 {
		t.Errorf("cold.go fan-in = %d, want 0", byFile["cold.go"].FanIn)
	}
	// caller1.go references hot.go but nothing references it.
	if byFile["caller1.go"].FanIn != 0 {
		t.Errorf("caller1.go fan-in = %d — outbound references must not count", byFile["caller1.go"].FanIn)
	}
}

func TestDefectRiskRanksAndExplains(t *testing.T) {
	kg := riskGraph(t)
	h := DefectHistory{
		Fixes:   map[string]int{"hot.go": 5, "cold.go": 1},
		Churn:   map[string]int{"hot.go": 6, "cold.go": 8},
		Authors: map[string]int{"hot.go": 4, "cold.go": 1},
		LastFix: map[string]time.Time{"hot.go": time.Now()},
	}
	risks := kg.DefectRisk(h, 0)
	if len(risks) != 2 || risks[0].File != "hot.go" {
		t.Fatalf("ranking = %+v, want hot.go first", risks)
	}
	if risks[0].Score <= risks[1].Score {
		t.Error("scores not ordered")
	}
	// Every score must be interrogable.
	for _, r := range risks {
		if len(r.Reasons) == 0 {
			t.Errorf("%s has a score with no reasons", r.File)
		}
	}
	if risks[0].Score > 1 {
		t.Errorf("score %.3f exceeds 1", risks[0].Score)
	}
}

// Files with no fix history are not defect-prone, and tests are excluded.
func TestDefectRiskExcludesCleanAndTestFiles(t *testing.T) {
	kg := riskGraph(t)
	h := DefectHistory{
		Fixes: map[string]int{"hot.go": 2, "thing_test.go": 9},
		Churn: map[string]int{"hot.go": 3, "thing_test.go": 10, "cold.go": 5},
	}
	for _, r := range kg.DefectRisk(h, 0) {
		if r.File == "cold.go" {
			t.Error("a file with no fixes should not be listed")
		}
		if r.File == "thing_test.go" {
			t.Error("test files should not be ranked as defect-prone")
		}
	}
}

// Proximity to the failure must matter, not just history.
func TestRankRootCausesWeightsDistance(t *testing.T) {
	kg := riskGraph(t)
	h := DefectHistory{
		Fixes: map[string]int{"hot.go": 5, "cold.go": 5},
		Churn: map[string]int{"hot.go": 6, "cold.go": 6},
	}
	// caller1.go fails; hot.go is one hop away, cold.go is unreachable.
	causes := kg.RankRootCauses(h, []string{"caller1.go"}, 0)
	if len(causes) == 0 {
		t.Fatal("no candidates returned")
	}

	rank := map[string]int{}
	for i, c := range causes {
		rank[c.File] = i
	}
	hotRank, hotFound := rank["hot.go"]
	coldRank, coldFound := rank["cold.go"]
	if !hotFound {
		t.Fatal("the reachable, defect-prone file was not ranked")
	}
	if coldFound && coldRank < hotRank {
		t.Error("an unreachable file outranked a reachable one with the same history")
	}
	for _, c := range causes {
		if len(c.Reasons) == 0 {
			t.Errorf("%s ranked with no explanation", c.File)
		}
	}
}
