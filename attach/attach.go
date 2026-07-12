// Package attach resolves user-supplied attachments (files, directories,
// images, URLs, inline text) into a markdown block that is prepended to a
// chat query so the agent operates with that material in context.
//
// It is shared by both the CLI console (which parses @Type:ref tokens out of
// the prompt) and the HTTP server (which receives a JSON attachments array
// from the GUI). Both produce []Attachment, call Resolve, and splice the
// returned block in front of the user's query.
package attach

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkcode/safeurl"
	"time"
)

// Type constants for the supported attachment kinds.
const (
	TypeFile      = "file"
	TypeDirectory = "directory"
	TypeImage     = "image"
	TypeURL       = "url"
	TypeText      = "text"
)

// KnownTypes is the set of type strings (lower-cased) accepted by Resolve.
// The CLI/GUI may present these with friendlier labels.
var KnownTypes = map[string]string{
	TypeFile:      "File",
	TypeDirectory: "Directory",
	TypeImage:     "Image",
	TypeURL:       "URL",
	TypeText:      "Text",
	// Common aliases.
	"dir":    "Directory",
	"folder": "Directory",
	"img":    "Image",
	"link":   "URL",
	"note":   "Text",
}

// Attachment is a single user-provided reference. Path is resolved against
// the active workspace; Content is used verbatim for TypeText and holds the
// URL for TypeURL.
type Attachment struct {
	Type    string `json:"type"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
}

// Result is the outcome of resolving one Attachment.
type Result struct {
	Type   string `json:"type"`
	Source string `json:"source"` // the original path/url/text (truncated for display)
	OK     bool   `json:"ok"`
	Body   string `json:"body,omitempty"`  // the text injected into the query
	Error  string `json:"error,omitempty"` // present when !OK
	Bytes  int    `json:"bytes,omitempty"` // size of file/url payload
}

// Resolve turns a list of attachments into a markdown "## Attachments" block.
// Relative paths are resolved against workspace (which may be "" to mean the
// process cwd). The block is returned empty if there are no attachments.
func Resolve(attachments []Attachment, workspace string) (string, []Result) {
	if len(attachments) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("\n## Attachments\n")
	results := make([]Result, 0, len(attachments))
	for _, a := range attachments {
		r := resolveOne(a, workspace)
		results = append(results, r)
		label := strings.ToUpper(r.Type[:1]) + r.Type[1:]
		b.WriteString("\n### " + label + ": " + truncForHeading(r.Source) + "\n")
		if !r.OK {
			b.WriteString("(could not attach: " + r.Error + ")\n")
			continue
		}
		switch a.Type {
		case TypeFile:
			// Fenced block; detect a language hint from the extension.
			lang := strings.TrimPrefix(filepath.Ext(a.Path), ".")
			b.WriteString("```" + lang + "\n" + r.Body + "\n```\n")
		case TypeURL:
			b.WriteString("```web\n" + r.Body + "\n```\n")
		case TypeText:
			b.WriteString("```text\n" + r.Body + "\n```\n")
		case TypeDirectory:
			b.WriteString("```\n" + r.Body + "\n```\n")
		case TypeImage:
			b.WriteString(r.Body + "\n")
		default:
			b.WriteString(r.Body + "\n")
		}
	}
	b.WriteString("\n## Task\n")
	return b.String(), results
}

// resolveOne resolves a single attachment.
func resolveOne(a Attachment, workspace string) Result {
	t := strings.ToLower(strings.TrimSpace(a.Type))
	if canon, ok := KnownTypes[t]; ok {
		// keep the canonical type for output
		t = strings.ToLower(canon)
	}
	r := Result{Type: t, Source: a.Path}
	if a.Type == TypeText || t == TypeText {
		r.Source = "inline text"
	} else if a.Type == TypeURL || t == TypeURL {
		r.Source = a.Content
		if r.Source == "" {
			r.Source = a.Path
		}
	}
	switch t {
	case TypeFile:
		body, n, err := readFileAttachment(a.Path, workspace)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.OK, r.Body, r.Bytes = true, body, n
		return r
	case TypeDirectory:
		body, err := readDirAttachment(a.Path, workspace)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.OK, r.Body = true, body
		return r
	case TypeImage:
		body, n, err := readImageAttachment(a.Path, workspace)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.OK, r.Body, r.Bytes = true, body, n
		return r
	case TypeURL:
		url := a.Content
		if url == "" {
			url = a.Path
		}
		r.Source = url
		body, n, err := readURLAttachment(url)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.OK, r.Body, r.Bytes = true, body, n
		return r
	case TypeText:
		r.OK, r.Body = true, a.Content
		return r
	default:
		r.Error = "unknown attachment type: " + a.Type
		return r
	}
}

// resolvePath turns a possibly-relative path into an absolute one against the
// workspace. Absolute paths are used as-is.
func resolvePath(p, workspace string) string {
	if p == "" {
		return workspace
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	base := workspace
	if base == "" {
		base, _ = os.Getwd()
	}
	return filepath.Clean(filepath.Join(base, p))
}

// readFileAttachment reads a file. Text files are returned as-is (truncated
// to maxFileBytes); binary files are summarised as a note.
func readFileAttachment(p, workspace string) (string, int, error) {
	abs := resolvePath(p, workspace)
	info, err := os.Stat(abs)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", p, err)
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("%s is a directory, not a file", p)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", p, err)
	}
	n := len(data)
	if isBinary(data) {
		return fmt.Sprintf("[binary file: %s — %s, %d bytes]", filepath.Base(abs), filepath.Ext(abs), n), n, nil
	}
	text := string(data)
	if len(text) > maxFileBytes {
		text = text[:maxFileBytes] + fmt.Sprintf("\n\n... [truncated, %d bytes total]", n)
	}
	return text, n, nil
}

// readImageAttachment notes an image's metadata. The agent runtime here is
// text-only, so we surface the path + size rather than embedding bytes.
func readImageAttachment(p, workspace string) (string, int, error) {
	abs := resolvePath(p, workspace)
	info, err := os.Stat(abs)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", p, err)
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("%s is a directory, not an image", p)
	}
	return fmt.Sprintf("[image attachment: %s — %s, %d bytes. The agent can read this file with the read_file tool if needed.]",
		filepath.Base(abs), filepath.Ext(abs), info.Size()), int(info.Size()), nil
}

// readDirAttachment produces a depth-limited tree listing of a directory.
func readDirAttachment(p, workspace string) (string, error) {
	abs := resolvePath(p, workspace)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", p, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file, not a directory", p)
	}
	var b strings.Builder
	b.WriteString(relOrAbs(abs, workspace) + "/\n")
	walkDirTree(&b, abs, "", 0, maxDirDepth)
	out := b.String()
	if len(out) > maxFileBytes {
		out = out[:maxFileBytes] + "\n... [truncated]"
	}
	return out, nil
}

// readURLAttachment fetches a URL and returns its (truncated) text body.
func readURLAttachment(url string) (string, int, error) {
	if url == "" {
		return "", 0, fmt.Errorf("empty url")
	}
	if !safeurl.IsSafeFetchURL(url, false) {
		return "", 0, fmt.Errorf("blocked: url targets a loopback, link-local, or private address (SSRF guard)")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "darkcode/attach")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxURLBytes))
	if err != nil {
		return "", 0, fmt.Errorf("read body: %w", err)
	}
	n := len(body)
	if resp.StatusCode >= 400 {
		return "", 0, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	text := string(body)
	if isBinary(body) {
		return fmt.Sprintf("[URL %s returned binary content (%d bytes)]", url, n), n, nil
	}
	if len(text) > maxFileBytes {
		text = text[:maxFileBytes] + fmt.Sprintf("\n\n... [truncated, %d bytes total]", n)
	}
	return text, n, nil
}

// walkDirTree appends a depth-limited tree of dir to b.
func walkDirTree(b *strings.Builder, dir, prefix string, depth, maxDepth int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Sort: directories first, then alphabetical.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	for i, e := range entries {
		name := e.Name()
		if skipDirs[name] {
			continue
		}
		if strings.HasPrefix(name, ".") && name != ".config" {
			continue
		}
		last := i == lastVisibleIndex(entries)
		connector := "├── "
		if last {
			connector = "└── "
		}
		b.WriteString(prefix + connector + name)
		if e.IsDir() {
			b.WriteString("/")
		}
		b.WriteString("\n")
		if e.IsDir() && depth < maxDepth {
			childPrefix := prefix
			if last {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}
			walkDirTree(b, filepath.Join(dir, name), childPrefix, depth+1, maxDepth)
		}
	}
}

func lastVisibleIndex(entries []os.DirEntry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if skipDirs[name] {
			continue
		}
		if strings.HasPrefix(name, ".") && name != ".config" {
			continue
		}
		return i
	}
	return 0
}

func relOrAbs(abs, workspace string) string {
	if workspace == "" {
		return abs
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		return abs
	}
	return rel
}

func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

func truncForHeading(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

const (
	maxFileBytes = 60_000
	maxURLBytes  = 200_000
	maxDirDepth  = 3
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "__pycache__": true, ".cache": true,
}
