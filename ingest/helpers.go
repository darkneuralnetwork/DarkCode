package ingest

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
)

// chunk splits text into chunks of about size bytes with overlap bytes carried
// from the end of one chunk into the start of the next. It prefers to break on
// a paragraph or line boundary within the last ~20% of a chunk so chunks don't
// slice mid-sentence when a natural break is nearby.
func chunk(text string, size, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= size {
		return []string{text}
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 8
	}
	var chunks []string
	for start := 0; start < len(text); {
		end := start + size
		if end >= len(text) {
			chunks = append(chunks, strings.TrimSpace(text[start:]))
			break
		}
		// Look for a natural break in the last 20% of the window.
		breakAt := end
		windowStart := end - size/5
		if windowStart < start {
			windowStart = start
		}
		if i := strings.LastIndex(text[windowStart:end], "\n\n"); i >= 0 {
			breakAt = windowStart + i
		} else if i := strings.LastIndexByte(text[windowStart:end], '\n'); i >= 0 {
			breakAt = windowStart + i
		}
		if breakAt <= start {
			breakAt = end
		}
		chunks = append(chunks, strings.TrimSpace(text[start:breakAt]))
		next := breakAt - overlap
		if next <= start {
			next = breakAt
		}
		start = next
	}
	// Drop any empties produced by trimming.
	out := chunks[:0]
	for _, c := range chunks {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// codeExtensions are treated as source code (category "code", KG-indexable).
var codeExtensions = map[string]bool{
	".go": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".py": true, ".rs": true, ".java": true, ".c": true, ".cc": true,
	".cpp": true, ".h": true, ".hpp": true, ".rb": true, ".php": true,
	".cs": true, ".swift": true, ".kt": true, ".scala": true, ".sh": true,
	".sql": true,
}

// docExtensions are treated as documentation/prose (category "doc").
var docExtensions = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".rst": true,
	".adoc": true, ".org": true, ".text": true, ".json": true,
	".yaml": true, ".yml": true, ".toml": true, ".csv": true,
	".html": true, ".xml": true,
}

func isCodeExt(path string) bool {
	return codeExtensions[strings.ToLower(filepath.Ext(path))]
}

// ingestibleExt reports whether a file's extension is a known text/code type
// worth ingesting. Unknown extensions (including no extension) are skipped to
// avoid pulling in binaries and lock files.
func ingestibleExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return codeExtensions[ext] || docExtensions[ext]
}

// skipDir reports whether a directory should not be descended into during a
// repo walk (VCS internals, dependency and build dirs, caches).
func skipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build",
		"target", ".venv", "venv", "__pycache__", ".idea", ".vscode",
		".darkcode", ".cache", "bin", "obj":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

// isBinary reports whether data looks binary (contains a NUL in the first 8KB).
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func newGetRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "DarkCode-Ingest/1.0")
	return req, nil
}
