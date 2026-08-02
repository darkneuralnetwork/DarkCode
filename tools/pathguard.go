package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// confineWrite enforces path confinement for file-mutating tools.
//
// A write/patch/replace must land inside the confinement root. This is
// defense-in-depth behind the permission gate: it stops a path traversal
// (`../../etc/crontab`), an absolute path (`/etc/passwd`), or a `~/…` write
// from escaping the directory being worked in — the move a prompt-injection
// payload would attempt. The check operates on the already resolved
// (expandPath'd) target.
//
// The root is the active workspace (core.WorkspaceKey) when one is set, and
// otherwise the process working directory.
//
// That fallback closes a real hole. The guard used to return nil when no
// workspace was in context, and core.WorkspaceKey was injected only by the HTTP
// handlers and the ACP entry point — never anywhere on the CLI path. So across
// the entire terminal surface, including single-query runs, this check did
// nothing and the agent could write /etc or ~/.ssh/authorized_keys. The
// filesystem sandbox does not cover it: that confines shell commands, while
// write_file, patch and replace_file_content are native Go and reach the disk
// through here alone.
//
// Defaulting to the working directory keeps every legitimate use working — an
// agent started inside a repository can still write throughout it — while
// making "no workspace configured" mean confined-to-here instead of
// unconfined. A guard whose default is to permit everything is not a guard, and
// the surfaces that know better still set the workspace explicitly.
func confineWrite(ctx context.Context, resolved string) error {
	root := CurrentWorkspace(ctx)
	label := "active workspace"
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			// Fail closed: without a root the write cannot be bounded, and
			// permitting it would reopen exactly what this closes.
			return fmt.Errorf("refusing write to %q: no workspace is set and the working directory could not be resolved: %w", resolved, err)
		}
		root, label = cwd, "working directory"
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("cannot resolve %s root: %w", label, err)
	}
	rootAbs = resolveSymlinks(rootAbs)
	target, err := filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("cannot resolve target path: %w", err)
	}
	// Resolve symlinks so a link inside the root pointing at, say, /etc can't
	// smuggle a write outside it. filepath.Abs only collapses "..".
	target = resolveSymlinks(target)
	// A path is inside the root iff its relative path from it is neither ".."
	// nor starts with "../".
	rel, err := filepath.Rel(rootAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q is outside the %s %q (blocked by path confinement)", resolved, label, rootAbs)
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
