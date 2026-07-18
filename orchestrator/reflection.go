package orchestrator

// reflection.go — deterministic reflection (local-first upgrade §7 Phase D).
//
// "Reflection: record *why* a task succeeded/failed → reusable procedural
// knowledge (the `Lessons` field exists but is unpopulated)." Deliberately
// rules-based, no LLM call: a reflection runs on EVERY task (including the
// trivial ones the cascade already answers for free), so spending an LLM
// call to generate it would undercut the whole local-first thesis. The
// signal is cheap and already on hand — goal keywords, tool usage, strategy,
// success, and a lookback over recent episodic memory for a prior failure on
// a similar goal (the strongest, cheapest "this fixed something" signal
// available without asking a model to guess).

import (
	"fmt"
	"strings"
	"time"

	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/memory"
)

// ReflectionKind classifies what a reflection produced, for KG fact typing.
type ReflectionKind string

const (
	ReflectionNone     ReflectionKind = ""
	ReflectionFix      ReflectionKind = "fix"
	ReflectionDecision ReflectionKind = "decision"
)

// Reflection is the output of reflecting on one completed task.
type Reflection struct {
	Kind    ReflectionKind
	Lessons []string
	// ProblemGoal is the prior failing task's goal text, set only when
	// Kind==ReflectionFix and a matching failure was found in episodic
	// memory. Empty otherwise (a "fix"-flavored success with no prior
	// failure on record still gets Kind==ReflectionFix from keywords alone,
	// but without a problem→fix edge to write — see populateKnowledgeGraph).
	ProblemGoal string
	// Similarity/FileOverlap/MatchAge are the corroborating evidence behind
	// ProblemGoal (only meaningful when ProblemGoal != ""), used by
	// populateKnowledgeGraph to grade how much the write-time confidence of
	// the resulting fix fact should reflect actual evidence strength rather
	// than a flat pass/fail threshold — see findRecentFailure.
	Similarity  float64
	FileOverlap bool
	MatchAge    time.Duration
}

// reflectFixKeywords / reflectDecisionKeywords classify a goal's flavor for
// KG fact typing. Deliberately narrower than router's classifier keyword
// sets (that one routes execution; this one decides what KIND of durable
// fact a completed task is worth recording as).
var (
	reflectFixKeywords = []string{
		"fix", "bug", "error", "crash", "broken", "issue", "debug", "regression",
	}
	reflectDecisionKeywords = []string{
		"design", "architect", "decide", "decision", "choose", "trade-off",
		"tradeoff", "which approach", "strategy for", "should we",
	}
)

// reflectLookbackWindow bounds how old a prior failure can be and still
// count as "fixed by" the current success — a match from months ago is more
// likely coincidental goal rephrasing than a real fix.
const reflectLookbackWindow = 7 * 24 * time.Hour

// reflectFixSimilarity is the GoalSimilarity threshold for treating a prior
// failure as "the same problem" this task just solved. Reuses the cascade's
// re-ask threshold (0.6) — the two use the same "same problem, different
// wording" judgment call, just with opposite success outcomes.
const reflectFixSimilarity = 0.6

// reflectLookbackScan bounds how many recent episodic entries are scanned
// for a prior failure, so reflection stays O(1)-ish even with a large
// episodic history.
const reflectLookbackScan = 50

// reflect derives a Reflection for a completed task from data already on
// hand — no LLM call. toolsUsed should be the deduplicated-or-not tool name
// list already computed by the caller (recordOutcome).
func (k *Kernel) reflect(goal, output string, toolsUsed []string, success bool, strategy string) Reflection {
	goalLower := strings.ToLower(goal)

	if !success {
		lessons := []string{fmt.Sprintf("Failed using strategy %q", strategy)}
		if len(toolsUsed) > 0 {
			lessons = append(lessons, "Tools attempted: "+strings.Join(dedupPreserveOrder(toolsUsed), ", "))
		}
		return Reflection{Kind: ReflectionNone, Lessons: lessons}
	}

	// Prior-failure lookback: the strongest available "this is a fix" signal
	// — cheaper and more reliable than keyword-guessing, and works even when
	// the goal text doesn't contain an obvious fix keyword.
	if ev := k.findRecentFailure(goal, output); ev.ProblemGoal != "" {
		lessons := []string{
			fmt.Sprintf("Fixed a previously failing task (%q) using strategy %q", strutil.Truncate(ev.ProblemGoal, 80), strategy),
		}
		if len(toolsUsed) > 0 {
			lessons = append(lessons, "Resolved with tools: "+strings.Join(dedupPreserveOrder(toolsUsed), ", "))
		}
		return Reflection{
			Kind: ReflectionFix, Lessons: lessons, ProblemGoal: ev.ProblemGoal,
			Similarity: ev.Similarity, FileOverlap: ev.FileOverlap, MatchAge: ev.Age,
		}
	}

	for _, kw := range reflectFixKeywords {
		if strings.Contains(goalLower, kw) {
			lessons := []string{fmt.Sprintf("Resolved a %q-type task using strategy %q", kw, strategy)}
			if len(toolsUsed) > 0 {
				lessons = append(lessons, "Tools used: "+strings.Join(dedupPreserveOrder(toolsUsed), ", "))
			}
			return Reflection{Kind: ReflectionFix, Lessons: lessons}
		}
	}

	for _, kw := range reflectDecisionKeywords {
		if strings.Contains(goalLower, kw) {
			return Reflection{
				Kind:    ReflectionDecision,
				Lessons: []string{fmt.Sprintf("Decided an approach for %q using strategy %q", strutil.Truncate(goal, 80), strategy)},
			}
		}
	}

	lessons := []string{fmt.Sprintf("Succeeded using strategy %q", strategy)}
	if len(toolsUsed) > 0 {
		lessons = append(lessons, "Tools used: "+strings.Join(dedupPreserveOrder(toolsUsed), ", "))
	}
	return Reflection{Kind: ReflectionNone, Lessons: lessons}
}

// fixEvidence is the corroborating signal behind a matched prior failure:
// how similar the wording was, whether the fix touched the same file(s) as
// the problem, and how soon after the failure the fix landed. Used to grade
// write-time confidence instead of treating every match at the threshold the
// same as a near-exact, same-file, immediate retry.
type fixEvidence struct {
	ProblemGoal string
	Similarity  float64
	FileOverlap bool
	Age         time.Duration
}

// findRecentFailure scans recent episodic entries (most-recent-first, per
// System.EpisodicGet's contract) for a failed task whose goal is a near-match
// to goal, within reflectLookbackWindow, and returns the corroborating
// evidence for that match. Returns a zero-value fixEvidence (ProblemGoal=="")
// if none found. fixOutput is the current (successful) task's output, used
// to check file-path overlap against the matched failure's output. Excludes
// the entry that's about to be recorded for the current task (it doesn't
// exist yet at call time, so no self-match risk).
func (k *Kernel) findRecentFailure(goal, fixOutput string) fixEvidence {
	if k.memory == nil {
		return fixEvidence{}
	}
	entries := k.memory.EpisodicGet()
	now := time.Now()
	scanned := 0
	for _, e := range entries {
		if scanned >= reflectLookbackScan {
			break
		}
		scanned++
		if e.Outcome != "failure" {
			continue
		}
		age := now.Sub(e.Timestamp)
		if age > reflectLookbackWindow {
			break // most-recent-first: everything after this is older still
		}
		sim := memory.GoalSimilarity(goal, e.TaskGoal)
		if sim >= reflectFixSimilarity {
			return fixEvidence{
				ProblemGoal: e.TaskGoal,
				Similarity:  sim,
				FileOverlap: pathsOverlap(extractPaths(e.TaskGoal, e.Output), extractPaths(goal, fixOutput)),
				Age:         age,
			}
		}
	}
	return fixEvidence{}
}

// pathsOverlap reports whether any path appears in both lists.
func pathsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, p := range a {
		set[p] = true
	}
	for _, p := range b {
		if set[p] {
			return true
		}
	}
	return false
}

// dedupPreserveOrder removes duplicate strings, keeping first-seen order.
func dedupPreserveOrder(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
