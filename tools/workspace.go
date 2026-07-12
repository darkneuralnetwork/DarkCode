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
