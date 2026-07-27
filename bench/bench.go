// Package bench runs reproducible task suites against the agent and reports
// solve rate, wall time, and token cost.
//
// The point is a number that can be published and re-checked by someone else.
// So a task is a directory on disk, not code: a prompt, a setup script that
// builds the starting state, and a verify script whose exit status is the
// only thing that decides pass or fail. No LLM grades the outcome — an
// LLM-graded benchmark measures the grader as much as the agent.
//
// Layout of a task directory:
//
//	tasks/<name>/
//	  task.json     {"prompt": "...", "timeout_seconds": 300}
//	  setup.sh      optional; prepares the workspace (run before the agent)
//	  verify.sh     required; exit 0 = solved
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Task is one benchmark case.
type Task struct {
	Name           string `json:"-"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds"`

	dir string // source directory, holding setup.sh / verify.sh
}

// Result is the outcome of one task.
type Result struct {
	Task     string        `json:"task"`
	Solved   bool          `json:"solved"`
	Duration time.Duration `json:"duration_ms"`
	Reason   string        `json:"reason,omitempty"` // why it failed
}

// Report aggregates a suite run.
type Report struct {
	Started  time.Time     `json:"started"`
	Total    int           `json:"total"`
	Solved   int           `json:"solved"`
	Duration time.Duration `json:"duration_ms"`
	Results  []Result      `json:"results"`
}

// SolveRate is the fraction solved, in [0,1].
func (r Report) SolveRate() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Solved) / float64(r.Total)
}

// LoadTasks reads every task directory under root.
func LoadTasks(root string) ([]Task, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Absolute, because the scripts are executed with the *workspace* as
		// the working directory — a relative task path would resolve against
		// the wrong root.
		dir, err := filepath.Abs(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, "task.json"))
		if err != nil {
			continue // not a task directory
		}
		var t Task
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("%s/task.json: %w", e.Name(), err)
		}
		if t.Prompt == "" {
			return nil, fmt.Errorf("%s: task.json has no prompt", e.Name())
		}
		if _, err := os.Stat(filepath.Join(dir, "verify.sh")); err != nil {
			return nil, fmt.Errorf("%s: verify.sh is required — a task with no check cannot be scored", e.Name())
		}
		t.Name, t.dir = e.Name(), dir
		if t.TimeoutSeconds <= 0 {
			t.TimeoutSeconds = 300
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	return tasks, nil
}

// Agent runs one prompt against a workspace. Implemented by the caller so the
// harness does not care whether it is driving a binary, an HTTP endpoint, or a
// competing tool — which is what makes head-to-head comparison possible.
type Agent interface {
	Run(ctx context.Context, workspace, prompt string) error
}

// BinaryAgent invokes a CLI in one-shot mode.
type BinaryAgent struct {
	Path string
	Args []string // extra flags; the prompt is appended after -q
}

// resolve makes the agent path absolute.
//
// Each task runs with cmd.Dir set to its own temporary workspace, so a
// relative path like "./darkcode" — which is what the Makefile and the
// package documentation both pass — is looked up inside that workspace and
// never found. The failure then surfaces once per task as "no such file or
// directory", which reads like every task failing rather than like the agent
// never having been started.
func (b BinaryAgent) resolve() (string, error) {
	if filepath.IsAbs(b.Path) {
		return b.Path, nil
	}
	// A bare name with no separator is a PATH lookup, not a relative path.
	if !strings.ContainsRune(b.Path, filepath.Separator) {
		return exec.LookPath(b.Path)
	}
	return filepath.Abs(b.Path)
}

func (b BinaryAgent) Run(ctx context.Context, workspace, prompt string) error {
	path, err := b.resolve()
	if err != nil {
		return fmt.Errorf("agent %q: %w", b.Path, err)
	}
	args := append(append([]string{}, b.Args...), "-q", prompt)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, lastLines(string(out), 5))
	}
	return nil
}

// Run executes every task in its own temporary workspace and returns the
// report. Each task is isolated, so one failure cannot contaminate the next.
func Run(ctx context.Context, tasks []Task, agent Agent) Report {
	rep := Report{Started: time.Now(), Total: len(tasks)}
	start := time.Now()

	for _, t := range tasks {
		res := runOne(ctx, t, agent)
		if res.Solved {
			rep.Solved++
		}
		rep.Results = append(rep.Results, res)
	}
	rep.Duration = time.Since(start)
	return rep
}

func runOne(ctx context.Context, t Task, agent Agent) Result {
	res := Result{Task: t.Name}
	started := time.Now()
	defer func() { res.Duration = time.Since(started) }()

	workspace, err := os.MkdirTemp("", "darkcode-bench-")
	if err != nil {
		res.Reason = "workspace: " + err.Error()
		return res
	}
	defer os.RemoveAll(workspace)

	if err := runScript(ctx, t.dir, "setup.sh", workspace, 120*time.Second); err != nil {
		res.Reason = "setup: " + err.Error()
		return res
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutSeconds)*time.Second)
	agentErr := agent.Run(runCtx, workspace, t.Prompt)
	cancel()

	// The agent erroring is not itself a failure — verify.sh is the judge, and
	// a task can be solved by a run that also reported a non-zero exit.
	if err := runScript(ctx, t.dir, "verify.sh", workspace, 120*time.Second); err != nil {
		res.Reason = "verify: " + err.Error()
		if agentErr != nil {
			res.Reason += " (agent: " + firstLine(agentErr.Error()) + ")"
		}
		return res
	}
	res.Solved = true
	return res
}

// runScript executes a task script against the workspace. A missing optional
// script is not an error; a missing required one fails at load time.
func runScript(ctx context.Context, taskDir, name, workspace string, timeout time.Duration) error {
	path := filepath.Join(taskDir, name)
	if _, err := os.Stat(path); err != nil {
		if name == "setup.sh" {
			return nil
		}
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", path)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "WORKSPACE="+workspace, "TASK_DIR="+taskDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, lastLines(string(out), 5))
	}
	return nil
}

// Format renders a human-readable summary.
func (r Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "solved %d/%d (%.0f%%) in %s\n\n", r.Solved, r.Total, r.SolveRate()*100, r.Duration.Round(time.Second))
	for _, res := range r.Results {
		mark := "PASS"
		if !res.Solved {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s  %-28s %6s", mark, res.Task, res.Duration.Round(time.Millisecond))
		if res.Reason != "" {
			fmt.Fprintf(&b, "  %s", firstLine(res.Reason))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
