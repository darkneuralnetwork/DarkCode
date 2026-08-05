// Package repowalk decides which parts of a repository the agent should look at.
//
// # WHY THIS EXISTS
//
// Four packages carried their own list of directories to skip and they did not
// agree. ingest skipped seventeen names including the agent's own state
// directory; the knowledge-graph sync skipped five; the code indexer checked
// two by substring; and the file tools — list_dir and search — skipped nothing
// at all.
//
// The visible consequence: asked what was in a workspace, the agent listed its
// own memory stores, knowledge graph and spilled tool results back to itself.
// That is worse than untidy. Those files are large, they are machine-generated,
// and reading them spends context describing the agent's own bookkeeping
// instead of the user's code — and any belief formed from them is a belief
// about the observer rather than the subject.
//
// One predicate, used everywhere. It lives under internal/ rather than in
// tools or ingest because the code indexer cannot import either of those
// without a cycle, and a rule that some packages cannot reach is a rule that
// grows a fourth copy.
package repowalk

import "strings"

// skipNames are directories that never contain source worth reading: version
// control internals, dependency trees, build output, editor and language
// caches, and the agent's own state.
var skipNames = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "bower_components": true,
	"dist": true, "build": true, "target": true, "out": true, "obj": true, "bin": true,
	".venv": true, "venv": true, "__pycache__": true, ".mypy_cache": true,
	".pytest_cache": true, ".tox": true, ".gradle": true, ".next": true, ".nuxt": true,
	".idea": true, ".vscode": true, ".cache": true, ".terraform": true,

	// The agent's own memory, knowledge graph, checkpoints and spilled tool
	// results. Reading these back is how an agent forms beliefs about itself.
	".darkcode": true,
}

// SkipDir reports whether a directory should not be descended into.
//
// Hidden directories are skipped as a class, after the explicit list, because
// dotfiles are configuration and caches far more often than they are the code
// someone asked about. "." and ".." are not skipped: a walk rooted at "." must
// be able to start.
func SkipDir(name string) bool {
	if name == "." || name == ".." || name == "" {
		return false
	}
	if skipNames[name] {
		return true
	}
	return strings.HasPrefix(name, ".")
}

// SkipPath reports whether any segment of a slash-separated path is skippable.
// For callers that see a full path rather than one directory at a time.
func SkipPath(path string) bool {
	for _, seg := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if SkipDir(seg) {
			return true
		}
	}
	return false
}
