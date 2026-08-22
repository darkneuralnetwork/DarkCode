package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// confineWrite enforces workspace path confinement for file-mutating tools.
//
// When a project workspace is active (core.WorkspaceKey is set in ctx), a
// write/patch/replace must land inside that workspace subtree. This is
// defense-in-depth behind the permission gate: it stops a path-traversal
// (`../../etc/crontab`), an absolute path (`/etc/passwd`), or a `~/…` write
// from escaping the project the user is working on — the kind of move a
// prompt-injection payload would attempt. The check operates on the already
// resolved (expandPath'd) target.
//
// When no workspace is on the request it FAILS CLOSED.
//
// It used to return nil there — "nothing to confine to, so allow" — and a test
// asserted that as correct with the comment "preserves CLI behavior". It
// preserved more than that. Neither CLI surface ever set core.WorkspaceKey, so
// confinement was inert for every interactive and headless session, and POST
// /api/tools/execute built its own bare context and wrote outside the
// workspace on a live binary. The control was documented as working; the check
// that "verified" it wrote to /etc, which an unprivileged process cannot do
// whether or not any confinement exists — so the test could not tell the guard
// apart from file permissions.
//
// Every path now reaches a tool through uiport, which refuses a request with no
// workspace, so an empty one here means a caller that skipped the front door.
// That is precisely the case not to trust. A guard whose default is to permit
// is not a guard.
func confineWrite(ctx context.Context, resolved string) error {
	_, err := withinWorkspace(ctx, resolved)
	return err
}

// withinWorkspace is the containment test itself, separated from confineWrite
// so a read path can ask the same question.
//
// Reads are deliberately NOT confined — the permission gate decides what the
// user is willing to let the agent look at, and confining reads to the
// workspace would break reading a config in $HOME that the user approved.
// Persisting what was read is a different question with a different answer:
// noteFileObservation feeds file contents into the knowledge graph, where they
// outlive the turn the user approved. CodeQL flagged that read as
// go/path-injection and it was right — an approved one-off look at
// ~/.ssh/config should not become a durable belief.
//
// It returns the path it actually validated, and callers that go on to touch
// the file MUST use that return rather than the string they passed in. The two
// differ: what is checked here is absolute and symlink-resolved, so a link
// inside the workspace pointing at /etc passes the check under its own name and
// would be opened under the target's. Validating one string and operating on
// another is the whole shape of a TOCTOU, and it is also the reason a taint
// tracker cannot see this function as a barrier — the value it follows never
// passed through the check.
func withinWorkspace(ctx context.Context, resolved string) (string, error) {
	ws := CurrentWorkspace(ctx)
	if ws == "" {
		return "", fmt.Errorf("refusing %q: this request carries no active workspace, "+
			"so path confinement cannot be enforced", resolved)
	}
	wsAbs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("cannot resolve workspace root: %w", err)
	}
	wsAbs = resolveSymlinks(wsAbs)
	target, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("cannot resolve target path: %w", err)
	}
	// Resolve symlinks so a link inside the workspace pointing at, say, /etc
	// can't smuggle a write outside it. filepath.Abs only collapses "..".
	target = resolveSymlinks(target)
	// A path is inside the workspace iff its relative path from the root is
	// neither ".." nor starts with "../".
	rel, err := filepath.Rel(wsAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the active workspace %q (blocked by path confinement)", resolved, wsAbs)
	}
	// Rejoining from the workspace root rather than returning `target` keeps the
	// result rooted at a value the check just constrained.
	return filepath.Join(wsAbs, rel), nil
}

// resolveSymlinks returns p with symlinks resolved. A write target usually
// doesn't exist yet, so EvalSymlinks would fail on the whole path; instead we
// resolve the longest existing ancestor and re-attach the not-yet-created
// remainder. Falls back to the cleaned absolute path if nothing resolves.
func resolveSymlinks(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	dir, rest := filepath.Split(p)
	dir = filepath.Clean(dir)
	for dir != "" && dir != string(filepath.Separator) && dir != "." {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(dir)
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	return filepath.Clean(p)
}
