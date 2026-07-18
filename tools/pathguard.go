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
	target, err := filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("cannot resolve target path: %w", err)
	}
	// filepath.Abs already applies Clean, collapsing any "..". A path is inside
	// the workspace iff its relative path from the root neither is ".." nor
	// starts with "../".
	rel, err := filepath.Rel(wsAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q is outside the active workspace %q (blocked by path confinement)", resolved, wsAbs)
	}
	return nil
}
