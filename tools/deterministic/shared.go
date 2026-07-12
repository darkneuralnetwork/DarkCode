package deterministic

// shared.go — helpers used by all deterministic tools.
//
// The deterministic toolchain (spec §8) backs rename / references / imports /
// definitions / dependencies with real computation — ripgrep for text search
// and go/ast for Go structural operations — instead of the previous stubs that
// returned hardcoded strings ("Symbol renamed.", "References found.").

import (
	"context"
	"os/exec"
	"strings"

	"github.com/darkcode/core"
)

// workspaceRoot returns the active workspace directory from the context, or
// "." if none is set. Deterministic tools always operate within the workspace.
func workspaceRoot(ctx context.Context) string {
	if ws, ok := ctx.Value(core.WorkspaceKey).(string); ok && ws != "" {
		return ws
	}
	return "."
}

// rgAvailable reports whether ripgrep is installed. We fall back to grep when
// it isn't, so the deterministic toolchain works in minimal environments.
func rgAvailable() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

// ripgrepOrGrep runs a word-boundary search for `symbol` under `root` and
// returns matching "file:line:column:match" lines (ripgrep format) or the
// grep -rn equivalent. Used by references + rename (dry-run).
func ripgrepOrGrep(ctx context.Context, root, symbol, glob string) (string, error) {
	if strings.TrimSpace(symbol) == "" {
		return "", nil
	}
	// Word-boundary, no color, with line + column numbers. -w avoids matching
	// substrings of longer identifiers (the deterministic guarantee).
	args := []string{"-w", "--line-number", "--column", "--no-heading", "--color=never"}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	args = append(args, symbol, root)

	if rgAvailable() {
		out, err := exec.CommandContext(ctx, "rg", args...).CombinedOutput()
		// rg exits 1 on no matches — that's not an error for us.
		if err != nil && len(out) == 0 {
			return "", err
		}
		return string(out), nil
	}
	// grep fallback (no column numbers, but still word-boundary via -w).
	out, err := exec.CommandContext(ctx, "grep", "-rnw", "--color=never", symbol, root).CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", err
	}
	return string(out), nil
}

// truncateOutput caps a tool result so a huge workspace scan can't blow out
// the LLM context window.
func truncateOutput(s string) string {
	const max = 50000
	if len(s) > max {
		return s[:max] + "\n... (truncated, " + itoa(len(s)-max) + " more bytes)"
	}
	return s
}

// itoa avoids importing strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
