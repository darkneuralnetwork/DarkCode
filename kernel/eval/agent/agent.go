// Package agent scores agent-trajectory quality against a labelled corpus —
// the sibling harness to kernel/eval, which scores retrieval quality.
//
// # WHY THIS EXISTS
//
// kernel/eval answers "did the graph make recall better", offline, with a
// real number attached. Nothing in this codebase answered the equivalent
// question for the kernel's own behavior: "did this change to escalation
// policy, or this new self-critique pass, actually improve task outcomes,
// or just move cost around." That question got argued from reading a diff,
// the exact problem kernel/eval's own package comment names.
//
// # WHAT IT MEASURES, AND WHAT IT DELIBERATELY DOES NOT
//
// A task names a goal and the artifact(s) that must exist (and be non-empty)
// in the workspace once the goal is done — the same acceptance-checking
// primitive kernel/orchestrator's own plan.Node.Artifacts already uses
// (contract.go), reused here rather than reinvented, so a task's definition
// of "done" means the same thing in eval as it does in a real run. It
// deliberately does NOT grade the *quality* of an answer with a model judge:
// a judged score is an opinion with a confidence interval nobody publishes,
// and the acceptance-checking philosophy this whole codebase already commits
// to (kernel/loop's contract-first stop condition, kernel/candidate's
// verifier-first ranking) is evidence over opinion wherever evidence exists.
//
// # WHY THIS IS SEPARATE FROM kernel/eval
//
// kernel/eval tests retrieval in isolation, with no model call, so it costs
// nothing and never flakes on a rate limit. This package's Executor is a real
// Kernel.Execute — the thing under test needs a live model to run at all.
// That's why it's wired as its own opt-in `make eval-agent` target, never
// into `test`/`test-race`/`ci`: those must stay fast, free, and offline.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Task is one goal to execute and the artifacts that prove it was done.
type Task struct {
	ID string `json:"id"`
	// Goal is handed to the executor exactly as a user would type it.
	Goal string `json:"goal"`
	// Artifacts are workspace-relative paths that must exist and be
	// non-empty once the goal is done — checked with the identical logic
	// kernel/orchestrator's acceptance checker uses for plan.Node.Artifacts,
	// so a task's "done" and a real run's "done" are the same test.
	Artifacts []string `json:"artifacts"`
	// Note records why these artifacts prove the goal, so a task can be
	// argued with rather than taken on trust — mirrors kernel/eval.Query.Note.
	Note string `json:"note,omitempty"`
}

// Corpus is one labelled set of trajectory tasks.
type Corpus struct {
	Name  string `json:"name"`
	About string `json:"about"`
	Tasks []Task `json:"tasks"`
}

// Load reads a corpus directory (corpus.json), mirroring kernel/eval.Load.
func Load(dir string) (*Corpus, error) {
	b, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		return nil, err
	}
	var c Corpus
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if c.Name == "" {
		c.Name = filepath.Base(dir)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate rejects a corpus that cannot measure anything.
func (c *Corpus) validate() error {
	ids := map[string]bool{}
	for _, t := range c.Tasks {
		if t.ID == "" {
			return fmt.Errorf("%s: a task has no id", c.Name)
		}
		if ids[t.ID] {
			return fmt.Errorf("%s: duplicate task id %q", c.Name, t.ID)
		}
		ids[t.ID] = true
		if t.Goal == "" {
			return fmt.Errorf("%s: task %q has no goal", c.Name, t.ID)
		}
		if len(t.Artifacts) == 0 {
			return fmt.Errorf("%s: task %q has no artifacts to check — nothing would prove it ran", c.Name, t.ID)
		}
	}
	if len(c.Tasks) == 0 {
		return fmt.Errorf("%s: no tasks", c.Name)
	}
	return nil
}

func sortedFailureIDs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
