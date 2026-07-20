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
// When no workspace is active (e.g. plain CLI use with no project) it is a
// no-op, preserving the pre-existing unconfined behavior.
func confineWrite(ctx context.Context, resolved string) error {
	ws := CurrentWorkspace(ctx)
	if ws == "" {
		return nil
	}
	wsAbs, err := filepath.Abs(ws)
	if err != nil {
		return fmt.Errorf("cannot resolve workspace root: %w", err)
	}
	wsAbs = resolveSymlinks(wsAbs)
	target, err := filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("cannot resolve target path: %w", err)
	}
	// Resolve symlinks so a link inside the workspace pointing at, say, /etc
	// can't smuggle a write outside it. filepath.Abs only collapses "..".
	target = resolveSymlinks(target)
	// A path is inside the workspace iff its relative path from the root is
	// neither ".." nor starts with "../".
	rel, err := filepath.Rel(wsAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q is outside the active workspace %q (blocked by path confinement)", resolved, wsAbs)
	}
	return nil
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
