package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

const corpusDir = "corpus/smoke-v1"

func load(t *testing.T) *Corpus {
	t.Helper()
	c, err := Load(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCorpusIsWellFormed mirrors kernel/eval's identically-named test: every
// other result is meaningless if a task has no artifacts to check.
func TestCorpusIsWellFormed(t *testing.T) {
	c := load(t)
	if len(c.Tasks) == 0 {
		t.Fatal("corpus has no tasks")
	}
	for _, task := range c.Tasks {
		if task.Note == "" {
			t.Errorf("task %q has no note — an unexplained task is a task nobody can argue with", task.ID)
		}
	}
}

func TestGoldTaskWithNoArtifactsIsRejected(t *testing.T) {
	c := &Corpus{Name: "broken", Tasks: []Task{{ID: "t1", Goal: "do a thing"}}}
	if err := c.validate(); err == nil {
		t.Fatal("a task with no artifacts passed validation")
	}
}

func TestDuplicateTaskIDIsRejected(t *testing.T) {
	c := &Corpus{Name: "broken", Tasks: []Task{
		{ID: "t1", Goal: "a", Artifacts: []string{"x"}},
		{ID: "t1", Goal: "b", Artifacts: []string{"y"}},
	}}
	if err := c.validate(); err == nil {
		t.Fatal("a duplicate task id passed validation")
	}
}

// scriptedExecutor is a fake Executor whose behavior per goal is scripted —
// the agent-trajectory counterpart of kernel/eval's fakeRetriever. Real
// eval-agent runs use a live *orchestrator.Kernel; this proves the harness
// mechanism (workspace isolation, artifact scoring, aggregation) is correct
// without needing a model call.
type scriptedExecutor struct {
	// write, if set for a goal, creates these workspace-relative files
	// (name -> content) when Execute is called for that goal.
	write map[string]map[string]string
	// failGoals, if a goal is a key, makes Execute return that error instead.
	failGoals map[string]error
}

func (s scriptedExecutor) Execute(ctx context.Context, goal string) (string, error) {
	if err, ok := s.failGoals[goal]; ok {
		return "", err
	}
	ws := core.WorkspaceFrom(ctx)
	for name, content := range s.write[goal] {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0644); err != nil {
			return "", err
		}
	}
	return "done", nil
}

// TestRunScoresArtifactsCorrectly proves Run's pass/fail logic: a task whose
// executor writes all expected artifacts passes; one that writes nothing, or
// errors, fails and names why.
func TestRunScoresArtifactsCorrectly(t *testing.T) {
	c := &Corpus{Name: "hand", Tasks: []Task{
		{ID: "passes", Goal: "write ok.txt", Artifacts: []string{"ok.txt"}},
		{ID: "missing-artifact", Goal: "do nothing useful", Artifacts: []string{"never-written.txt"}},
		{ID: "executor-errors", Goal: "this will fail", Artifacts: []string{"x.txt"}},
		{ID: "empty-artifact", Goal: "write an empty file", Artifacts: []string{"empty.txt"}},
	}}
	exec := scriptedExecutor{
		write: map[string]map[string]string{
			"write ok.txt":        {"ok.txt": "content"},
			"write an empty file": {"empty.txt": ""},
			"do nothing useful":   {},
		},
		failGoals: map[string]error{
			"this will fail": errors.New("simulated executor failure"),
		},
	}

	s, err := Run(context.Background(), "scripted", c, exec, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if s.Tasks != 4 {
		t.Fatalf("Tasks = %d, want 4", s.Tasks)
	}
	if s.Passed != 1 {
		t.Errorf("Passed = %d, want 1 (only \"passes\" should pass): failures=%v", s.Passed, s.Failures)
	}
	if s.PassRate != 0.25 {
		t.Errorf("PassRate = %.3f, want 0.25", s.PassRate)
	}
	if _, ok := s.Failures["missing-artifact"]; !ok {
		t.Error("missing-artifact should have failed (artifact never written)")
	}
	if reason := s.Failures["executor-errors"]; reason == "" {
		t.Error("executor-errors should have failed (executor returned an error)")
	}
	if reason := s.Failures["empty-artifact"]; reason == "" {
		t.Error("empty-artifact should have failed (artifact exists but is empty)")
	}
	if _, failed := s.Failures["passes"]; failed {
		t.Error("\"passes\" task was recorded as a failure")
	}
}

// TestRunIsolatesTaskWorkspaces proves two tasks writing files with the same
// name don't collide — each task gets its own scratch subdirectory.
func TestRunIsolatesTaskWorkspaces(t *testing.T) {
	c := &Corpus{Name: "hand", Tasks: []Task{
		{ID: "task-a", Goal: "write shared.txt (a)", Artifacts: []string{"shared.txt"}},
		{ID: "task-b", Goal: "write shared.txt (b)", Artifacts: []string{"shared.txt"}},
	}}
	exec := scriptedExecutor{write: map[string]map[string]string{
		"write shared.txt (a)": {"shared.txt": "from a"},
		"write shared.txt (b)": {"shared.txt": "from b"},
	}}

	root := t.TempDir()
	s, err := Run(context.Background(), "scripted", c, exec, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Passed != 2 {
		t.Fatalf("Passed = %d, want 2 (isolated workspaces should let both write \"shared.txt\" without colliding): failures=%v", s.Passed, s.Failures)
	}
	a, err := os.ReadFile(filepath.Join(root, "task-a", "shared.txt"))
	if err != nil || string(a) != "from a" {
		t.Errorf("task-a's shared.txt = %q, %v, want \"from a\"", a, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "task-b", "shared.txt"))
	if err != nil || string(b) != "from b" {
		t.Errorf("task-b's shared.txt = %q, %v, want \"from b\"", b, err)
	}
}

// TestScorecardMentionsFailures is a smoke test for the report renderer.
func TestScorecardMentionsFailures(t *testing.T) {
	c := &Corpus{Name: "hand", About: "a hand-built corpus", Tasks: []Task{
		{ID: "t1", Goal: "g", Artifacts: []string{"x"}},
	}}
	scores := []Score{{Adapter: "scripted", Tasks: 1, Passed: 0, PassRate: 0, Failures: map[string]string{"t1": "expected artifact x does not exist"}}}
	card := Scorecard(c, scores)
	for _, want := range []string{"hand", "scripted", "t1", "expected artifact x does not exist"} {
		if !strings.Contains(card, want) {
			t.Errorf("scorecard missing %q:\n%s", want, card)
		}
	}
}
