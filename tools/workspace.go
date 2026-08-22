package tools

import (
	"context"
	"path/filepath"

	"github.com/darkcode/core"
)

// CurrentWorkspace returns the active workspace directory from the context, or "" if none.
// It looks for core.WorkspaceKey in the context, which is injected by the server middleware
// or CLI based on the active project.
func CurrentWorkspace(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if ws, ok := ctx.Value(core.WorkspaceKey).(string); ok {
		return ws
	}
	return ""
}

// WithReadOnlyTools marks ctx so the registry refuses to run any mutating tool.
// Chat mode sets this so even if a write tool were somehow requested, it can't
// run — defense-in-depth behind not offering write tools to the model at all.
// It uses the shared core.ReadOnlyToolsKey so the ReAct loop (schema selection)
// and the registry (enforcement) agree on one key.
func WithReadOnlyTools(ctx context.Context) context.Context {
	return context.WithValue(ctx, core.ReadOnlyToolsKey, true)
}

// IsReadOnlyContext reports whether the request is under the read-only policy.
func IsReadOnlyContext(ctx context.Context) bool {
	return core.IsReadOnlyTools(ctx)
}

// resolveInWorkspace joins a relative path with the active workspace from the context.
// Absolute paths and empty paths are returned unchanged.
func resolveInWorkspace(ctx context.Context, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	ws := CurrentWorkspace(ctx)
	if ws == "" {
		return path
	}
	return filepath.Join(ws, path)
}
