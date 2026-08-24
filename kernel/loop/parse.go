package loop

// parse.go — letting the user say what "done" means.
//
// The contract machinery takes arbitrary acceptance criteria and decides
// completion by running them. Until now criteria could only come from the
// planner, so the one person who reliably knows when a task is finished — the
// person who asked for it — had no way to say so.
//
//	/loop until `go test ./...` passes: add retry logic to the HTTP client
//	/loop until src/index.html exists: build the landing page
//
// The value is not the syntax, it is what the syntax buys: the run ends on
// evidence the user chose. "Run until the task is finished" stops being
// something the tool infers from a model's opinion of its own work and becomes
// something stated up front and checked mechanically.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// untilSep separates the criterion from the task. A colon reads naturally and
// is rare in a shell command; a criterion that genuinely needs one can be
// backticked, since the backticks are stripped first.
const untilSep = ":"

// ParseUntil splits an inline acceptance criterion off the front of a goal.
//
//	"until `go test ./...` passes: add retries"  →  ("go test ./...", "add retries", true)
//
// Returns ok=false when there is no `until` prefix, when no separator follows
// it, or when either half is empty — a goal that is all criterion and no task
// is a mistake worth surfacing rather than silently running.
func ParseUntil(goal string) (criterion, task string, ok bool) {
	trimmed := strings.TrimSpace(goal)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "until ") {
		return "", "", false
	}
	rest := strings.TrimSpace(trimmed[len("until "):])

	// A backticked criterion may contain the separator, so consume it whole
	// before looking for one.
	if strings.HasPrefix(rest, "`") {
		if end := strings.Index(rest[1:], "`"); end >= 0 {
			criterion = strings.TrimSpace(rest[1 : 1+end])
			after := strings.TrimSpace(rest[1+end+1:])
			after = strings.TrimPrefix(after, "passes")
			after = strings.TrimSpace(after)
			if !strings.HasPrefix(after, untilSep) {
				return "", "", false
			}
			task = strings.TrimSpace(strings.TrimPrefix(after, untilSep))
			if criterion == "" || task == "" {
				return "", "", false
			}
			return criterion, task, true
		}
	}

	i := strings.Index(rest, untilSep)
	if i < 0 {
		return "", "", false
	}
	criterion = strings.TrimSpace(rest[:i])
	task = strings.TrimSpace(rest[i+1:])
	criterion = strings.TrimSuffix(strings.TrimSpace(criterion), "passes")
	criterion = strings.TrimSpace(criterion)
	if criterion == "" || task == "" {
		return "", "", false
	}
	return criterion, task, true
}

// FileCriterion reports whether a criterion names a file to exist rather than a
// command to run. "src/index.html exists" and "src/index.html" both qualify;
// "go test ./..." does not.
//
// Distinguishing them matters because the two are checked completely
// differently, and running "src/index.html" as a shell command would fail in a
// way that looks like the task failed.
func FileCriterion(criterion string) (path string, ok bool) {
	c := strings.TrimSpace(criterion)
	c = strings.TrimSuffix(c, " exists")
	c = strings.TrimSpace(c)
	if c == "" || strings.ContainsAny(c, " \t|&;><") {
		return "", false // a command, not a path
	}
	// A path worth checking either looks like one or has an extension.
	if strings.ContainsAny(c, "/\\") || filepath.Ext(c) != "" {
		return c, true
	}
	return "", false
}

// ContractFromUntil builds an enforceable contract from a user-stated
// criterion. run executes a shell command in the workspace and reports whether
// it succeeded along with its output; workspace resolves relative file paths.
func ContractFromUntil(criterion, workspace string, run func(cmd string) (bool, string)) *Contract {
	c := &Contract{Criteria: []string{criterion}}

	if path, isFile := FileCriterion(criterion); isFile {
		c.Artifacts = []string{path}
		c.Verify = func(context.Context) Verdict {
			full := path
			if !filepath.IsAbs(full) {
				full = filepath.Join(workspace, path)
			}
			info, err := os.Stat(full)
			switch {
			case err != nil:
				return Verdict{Checked: 1, Evidence: path + " does not exist"}
			case info.Size() == 0:
				return Verdict{Checked: 1, Evidence: path + " exists but is empty"}
			}
			return Verdict{Passed: true, Checked: 1}
		}
		return c
	}

	if run == nil {
		return c // criteria shown to the model, but nothing can enforce them
	}
	c.Verify = func(context.Context) Verdict {
		passed, out := run(criterion)
		v := Verdict{Passed: passed, Checked: 1}
		if !passed {
			v.Evidence = "$ " + criterion + "\n" + strings.TrimSpace(out)
		}
		return v
	}
	return c
}
