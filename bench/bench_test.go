package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTask lays down a task directory. setup may be empty.
func writeTask(t *testing.T, root, name, taskJSON, setup, verify string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(file, content string) {
		if content == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("task.json", taskJSON)
	write("setup.sh", setup)
	write("verify.sh", verify)
}

// agentFunc adapts a function to the Agent interface.
type agentFunc func(ctx context.Context, workspace, prompt string) error

func (f agentFunc) Run(ctx context.Context, workspace, prompt string) error {
	return f(ctx, workspace, prompt)
}

func TestLoadTasksReadsAndSorts(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "b-task", `{"prompt":"second"}`, "", "#!/bin/bash\ntrue\n")
	writeTask(t, root, "a-task", `{"prompt":"first","timeout_seconds":42}`, "", "#!/bin/bash\ntrue\n")

	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Name != "a-task" || tasks[1].Name != "b-task" {
		t.Errorf("tasks not sorted by name: %s, %s", tasks[0].Name, tasks[1].Name)
	}
	if tasks[0].TimeoutSeconds != 42 {
		t.Errorf("timeout = %d, want 42", tasks[0].TimeoutSeconds)
	}
	if tasks[1].TimeoutSeconds != 300 {
		t.Errorf("default timeout = %d, want 300", tasks[1].TimeoutSeconds)
	}
}

// A task with no verify script cannot be scored, so loading must refuse it
// rather than silently counting it as passed.
func TestLoadTasksRejectsUnscoreableTask(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "no-verify", `{"prompt":"x"}`, "", "")
	if _, err := LoadTasks(root); err == nil || !strings.Contains(err.Error(), "verify.sh") {
		t.Errorf("expected a verify.sh error, got %v", err)
	}

	root2 := t.TempDir()
	writeTask(t, root2, "no-prompt", `{}`, "", "#!/bin/bash\ntrue\n")
	if _, err := LoadTasks(root2); err == nil {
		t.Error("a task with no prompt should be rejected")
	}
}

// verify.sh is the sole judge: the agent's own exit status must not decide it.
func TestVerifyScriptDecidesOutcome(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "solved", `{"prompt":"make the file","timeout_seconds":30}`,
		"", "#!/usr/bin/env bash\ntest -f done.txt\n")
	writeTask(t, root, "unsolved", `{"prompt":"make the file","timeout_seconds":30}`,
		"", "#!/usr/bin/env bash\ntest -f never.txt\n")

	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatal(err)
	}

	// The agent creates done.txt and then reports an error anyway.
	agent := agentFunc(func(_ context.Context, workspace, _ string) error {
		if err := os.WriteFile(filepath.Join(workspace, "done.txt"), []byte("ok"), 0o644); err != nil {
			return err
		}
		return context.Canceled // a non-nil error must not veto a passing verify
	})

	rep := Run(context.Background(), tasks, agent)
	if rep.Total != 2 || rep.Solved != 1 {
		t.Fatalf("solved %d/%d, want 1/2: %+v", rep.Solved, rep.Total, rep.Results)
	}
	if rep.SolveRate() != 0.5 {
		t.Errorf("SolveRate = %v, want 0.5", rep.SolveRate())
	}
	for _, res := range rep.Results {
		if res.Task == "solved" && !res.Solved {
			t.Error("a passing verify.sh must count as solved despite the agent's error")
		}
		if res.Task == "unsolved" && res.Reason == "" {
			t.Error("a failure should carry a reason")
		}
	}
}

// setup.sh builds the starting state, and each task gets a clean workspace.
func TestSetupRunsAndWorkspacesAreIsolated(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "with-setup", `{"prompt":"x","timeout_seconds":30}`,
		"#!/usr/bin/env bash\necho seeded > seed.txt\n",
		"#!/usr/bin/env bash\ngrep -q seeded seed.txt && test -f agent.txt\n")

	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	agent := agentFunc(func(_ context.Context, workspace, _ string) error {
		seen = append(seen, workspace)
		// The setup script's output must be visible to the agent.
		if _, err := os.Stat(filepath.Join(workspace, "seed.txt")); err != nil {
			t.Errorf("setup output missing from the workspace: %v", err)
		}
		return os.WriteFile(filepath.Join(workspace, "agent.txt"), []byte("x"), 0o644)
	})

	rep := Run(context.Background(), tasks, agent)
	if rep.Solved != 1 {
		t.Fatalf("task not solved: %+v", rep.Results)
	}
	// Workspaces are temporary and removed afterwards.
	for _, ws := range seen {
		if _, err := os.Stat(ws); !os.IsNotExist(err) {
			t.Errorf("workspace %s was not cleaned up", ws)
		}
	}
}

func TestReportFormat(t *testing.T) {
	rep := Report{Total: 2, Solved: 1, Results: []Result{
		{Task: "a", Solved: true},
		{Task: "b", Solved: false, Reason: "verify: exit status 1"},
	}}
	out := rep.Format()
	for _, want := range []string{"solved 1/2", "50%", "PASS", "FAIL", "verify: exit status 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestEmptyReportSolveRate(t *testing.T) {
	if (Report{}).SolveRate() != 0 {
		t.Error("an empty report should score 0, not divide by zero")
	}
}

// The shipped suite must stay loadable — a malformed task should break CI
// here rather than during a benchmark run.
func TestShippedTasksAreValid(t *testing.T) {
	tasks, err := LoadTasks("tasks")
	if err != nil {
		t.Fatalf("shipped tasks do not load: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no shipped benchmark tasks found")
	}
	for _, task := range tasks {
		if task.Prompt == "" {
			t.Errorf("%s has an empty prompt", task.Name)
		}
	}
}
