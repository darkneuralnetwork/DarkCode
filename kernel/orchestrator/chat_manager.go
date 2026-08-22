package orchestrator

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// ChatManager centralizes per-request "shape" decisions and post-execution
// completeness checks. It is heuristic-first (no LLM calls) so it adds no cost;
// it mirrors the lightweight, stateless ErrorManager pattern.
//
// Today it focuses on deliverable-completeness: verifying that a Build actually
// produced the artifacts its goal implies (the "made a website but no .html/.js"
// class of failure), so the kernel/server can auto-continue to finish or mark a
// workflow subtask done only when its output really exists.
type ChatManager struct{}

// NewChatManager builds a ChatManager.
func NewChatManager() *ChatManager { return &ChatManager{} }

// artifactExpectation is a file artifact a goal implies.
type artifactExpectation struct {
	Ext      string // e.g. ".html"
	Label    string // human description, e.g. "an HTML page"
	Required bool   // a missing required artifact is a completeness gap
}

// ExpectedArtifacts heuristically maps a Build goal to the file artifacts it
// should produce. Deliberately conservative — only high-confidence mappings —
// so it flags genuine gaps ("website" with no .html) without false alarms.
func (cm *ChatManager) ExpectedArtifacts(goal string) []artifactExpectation {
	g := strings.ToLower(goal)
	var out []artifactExpectation
	have := map[string]bool{}
	add := func(ext, label string, req bool) {
		if have[ext] {
			return
		}
		have[ext] = true
		out = append(out, artifactExpectation{Ext: ext, Label: label, Required: req})
	}

	if containsAny(g, "website", "web site", "web page", "webpage", "web app", "landing page", "html page", " site ") {
		add(".html", "an HTML page", true)
		add(".css", "a CSS stylesheet", false)
		add(".js", "a JavaScript file", false)
	}
	if containsAny(g, "html") {
		add(".html", "an HTML page", true)
	}
	if containsAny(g, "stylesheet", " css ") {
		add(".css", "a CSS stylesheet", true)
	}
	if containsAny(g, "javascript", "js file", "js script") {
		add(".js", "a JavaScript file", true)
	}
	if containsAny(g, "python script", "python program", " a script in python") {
		add(".py", "a Python file", true)
	}
	return out
}

// CheckCompleteness reports whether a Build goal appears satisfied by the files
// now present in the workspace, returning human-readable gaps for anything
// required but missing. Pure heuristic (file existence) — no LLM call. A goal
// with no recognized artifact expectation is treated as complete (nothing to
// assert).
func (cm *ChatManager) CheckCompleteness(goal, workspace string) (done bool, gaps []string) {
	arts := cm.ExpectedArtifacts(goal)
	if len(arts) == 0 || strings.TrimSpace(workspace) == "" {
		return true, nil
	}
	present := workspaceExtensions(workspace)
	for _, a := range arts {
		if a.Required && !present[a.Ext] {
			gaps = append(gaps, a.Label+" ("+a.Ext+")")
		}
	}
	return len(gaps) == 0, gaps
}

// workspaceExtensions returns the set of file extensions present in workspace
// (bounded walk, skipping VCS/dependency/build dirs).
func workspaceExtensions(root string) map[string]bool {
	present := map[string]bool{}
	count := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipWorkspaceDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if count > 20000 { // safety bound on huge trees
			return filepath.SkipAll
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != "" {
			present[ext] = true
		}
		return nil
	})
	return present
}

func skipWorkspaceDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build",
		"target", ".venv", "venv", "__pycache__", ".idea", ".vscode",
		".darkcode", ".cache", "bin", "obj":
		return true
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
