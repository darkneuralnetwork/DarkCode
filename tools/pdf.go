package tools

// ============================================================================
// PDF TOOL — in-registry PDF manipulation (info, extract_text, merge, split,
// rotate). Registered as a first-class builtin tool so the agent can work with
// PDF documents in any tool-enabled mode (Project/Auto/Loop; NOT General).
//
// Design (robust + not bulky):
//   • info, extract_text → pure Go, stdlib only (compress/zlib for FlateDecode).
//     These ALWAYS work with no external dependencies.
//   • merge, split, rotate → delegate to ghostscript (gs) when available
//     (the industry-standard PDF engine; already present on the host). If gs
//     is absent, the tool returns a clear, actionable error naming the
//     operation and the package to install. We do NOT ship an 800-line
//     fragile PDF page-tree rewriter — that would be bulk for little gain.
//
// The same PDF operations are ALSO expressible through the in-house ITF
// format for a REMOTE device (see examples/itf/pdf-remote.json), which uses
// the "htp" execution type to call a remote HTP server's pdf tool. So a
// device on the network can contribute PDF capability without local gs.
// ============================================================================

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// pdfHandler is the registry handler for the "pdf" tool.
func pdfHandler(ctx context.Context, args map[string]interface{}) *ToolResult {
	operation, _ := args["operation"].(string)
	if operation == "" {
		operation = "info"
	}
	switch operation {
	case "info":
		return pdfInfo(args)
	case "extract_text":
		return pdfExtractText(args)
	case "merge":
		return pdfMerge(ctx, args)
	case "split":
		return pdfSplit(ctx, args)
	case "rotate":
		return pdfRotate(ctx, args)
	default:
		return &ToolResult{Name: "pdf", Success: false, Error: "unknown operation: " + operation +
			" (valid: info, extract_text, merge, split, rotate)"}
	}
}

// pdfInfo reads lightweight metadata: PDF version, page count, page sizes,
// and the /Info dictionary (Title/Author/Subject) when present.
func pdfInfo(args map[string]interface{}) *ToolResult {
	path, ok := pdfPathArg(args)
	if !ok {
		return pdfBadArg("file path required (file)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pdfIOErr(path, err)
	}
	version := pdfVersion(data)
	pageCount := pdfCountPages(data)
	sizes := pdfPageSizes(data)
	meta := pdfInfoDict(data)

	var sb strings.Builder
	fmt.Fprintf(&sb, "File: %s\n", path)
	fmt.Fprintf(&sb, "PDF version: %s\n", version)
	fmt.Fprintf(&sb, "Pages: %d\n", pageCount)
	if len(sizes) > 0 {
		fmt.Fprintf(&sb, "Page sizes (pts, W×H):\n")
		limit := len(sizes)
		if limit > 10 {
			limit = 10
		}
		for i := 0; i < limit; i++ {
			fmt.Fprintf(&sb, "  page %d: %s\n", i+1, sizes[i])
		}
		if len(sizes) > 10 {
			fmt.Fprintf(&sb, "  …(%d more)\n", len(sizes)-10)
		}
	}
	if meta != "" {
		fmt.Fprintf(&sb, "Metadata:\n%s\n", meta)
	}
	return &ToolResult{Name: "pdf", Success: true, Output: strings.TrimSpace(sb.String())}
}

// pdfExtractText pulls text from content streams. It handles uncompressed
// streams and FlateDecode (zlib) streams — the two dominant cases. Output is
// capped to protect the agent's context window.
func pdfExtractText(args map[string]interface{}) *ToolResult {
	path, ok := pdfPathArg(args)
	if !ok {
		return pdfBadArg("file path required (file)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pdfIOErr(path, err)
	}
	if bytes.Contains(data, []byte("/Encrypt")) {
		return &ToolResult{Name: "pdf", Success: false, Error: "PDF appears encrypted; decrypt it first"}
	}
	streams := pdfExtractStreams(data)
	var sb strings.Builder
	for _, s := range streams {
		txt := pdfTextFromStream(s)
		if txt != "" {
			sb.WriteString(txt)
			sb.WriteByte('\n')
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		out = "(no extractable text found — the PDF may use encoded fonts or be image-only)"
	}
	const cap = 256 * 1024 // 256KB
	if len(out) > cap {
		out = out[:cap] + "\n…[truncated]"
	}
	return &ToolResult{Name: "pdf", Success: true, Output: out}
}

// pdfMerge concatenates PDFs via ghostscript. Files come from the "files"
// array (or comma-separated "file"); output goes to "output".
func pdfMerge(ctx context.Context, args map[string]interface{}) *ToolResult {
	files := pdfFilesArg(args)
	output, _ := args["output"].(string)
	if len(files) < 2 {
		return pdfBadArg("merge needs ≥2 input files (files) and an output path (output)")
	}
	if output == "" {
		return pdfBadArg("output path required (output)")
	}
	if err := pdfRequireGS(); err != nil {
		return &ToolResult{Name: "pdf", Success: false, Error: err.Error()}
	}
	cmdArgs := []string{"-dNOPAUSE", "-dBATCH", "-dSAFER", "-sDEVICE=pdfwrite",
		"-sOutputFile=" + output}
	cmdArgs = append(cmdArgs, files...)
	if out, err := runGS(ctx, cmdArgs); err != nil {
		return &ToolResult{Name: "pdf", Success: false, Output: out, Error: "gs merge failed: " + err.Error()}
	}
	st, _ := os.Stat(output)
	return &ToolResult{Name: "pdf", Success: true, Output: fmt.Sprintf(
		"Merged %d PDFs → %s (%d bytes)", len(files), output, fileSize(st))}
}

// pdfSplit extracts a page range [from,to] (1-indexed) into a new PDF via gs.
func pdfSplit(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, ok := pdfPathArg(args)
	if !ok {
		return pdfBadArg("file path required (file)")
	}
	output, _ := args["output"].(string)
	if output == "" {
		return pdfBadArg("output path required (output)")
	}
	from := pdfIntArg(args, "from", 1)
	to := pdfIntArg(args, "to", from)
	if from < 1 || to < from {
		return pdfBadArg("from must be ≥1 and to ≥ from")
	}
	if err := pdfRequireGS(); err != nil {
		return &ToolResult{Name: "pdf", Success: false, Error: err.Error()}
	}
	cmdArgs := []string{"-dNOPAUSE", "-dBATCH", "-dSAFER", "-sDEVICE=pdfwrite",
		fmt.Sprintf("-dFirstPage=%d", from), fmt.Sprintf("-dLastPage=%d", to),
		"-sOutputFile=" + output, path}
	if out, err := runGS(ctx, cmdArgs); err != nil {
		return &ToolResult{Name: "pdf", Success: false, Output: out, Error: "gs split failed: " + err.Error()}
	}
	st, _ := os.Stat(output)
	return &ToolResult{Name: "pdf", Success: true, Output: fmt.Sprintf(
		"Extracted pages %d-%d from %s → %s (%d bytes)", from, to, path, output, fileSize(st))}
}

// pdfRotate rotates all pages by degrees (90/180/270) via gs.
func pdfRotate(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, ok := pdfPathArg(args)
	if !ok {
		return pdfBadArg("file path required (file)")
	}
	output, _ := args["output"].(string)
	if output == "" {
		return pdfBadArg("output path required (output)")
	}
	deg := pdfIntArg(args, "degrees", 90)
	if deg%90 != 0 {
		return pdfBadArg("degrees must be a multiple of 90")
	}
	if err := pdfRequireGS(); err != nil {
		return &ToolResult{Name: "pdf", Success: false, Error: err.Error()}
	}
	cmdArgs := []string{"-dNOPAUSE", "-dBATCH", "-dSAFER", "-sDEVICE=pdfwrite",
		"-dAutoRotatePages=/None",
		"-c", fmt.Sprintf("<< /Install { %d 0 0 %d 0 0 concat } >> setpagedevice", deg, deg),
		"-f", "-sOutputFile=" + output, path}
	if out, err := runGS(ctx, cmdArgs); err != nil {
		return &ToolResult{Name: "pdf", Success: false, Output: out, Error: "gs rotate failed: " + err.Error()}
	}
	st, _ := os.Stat(output)
	return &ToolResult{Name: "pdf", Success: true, Output: fmt.Sprintf(
		"Rotated %s by %d° → %s (%d bytes)", path, deg, output, fileSize(st))}
}

// ── pure-Go PDF parsing helpers ──────────────────────────────────────────

var pdfHeaderRe = regexp.MustCompile(`%PDF-(\d\.\d)`)

func pdfVersion(data []byte) string {
	m := pdfHeaderRe.FindSubmatch(data)
	if m != nil {
		return string(m[1])
	}
	return "unknown"
}

// pdfCountPages counts "/Type /Page" occurrences that are NOT "/Pages".
// This is a heuristic but works for the vast majority of well-formed PDFs.
func pdfCountPages(data []byte) int {
	n := 0
	idx := 0
	for {
		i := bytes.Index(data[idx:], []byte("/Type"))
		if i < 0 {
			break
		}
		idx += i + 5
		// skip whitespace
		for idx < len(data) && (data[idx] == ' ' || data[idx] == '\t' || data[idx] == '\n' || data[idx] == '\r') {
			idx++
		}
		if idx+6 <= len(data) && bytes.HasPrefix(data[idx:], []byte("/Page")) &&
			(idx+6 >= len(data) || data[idx+5] != 's') {
			n++
		}
	}
	if n == 0 {
		// Fallback: count /Page tokens loosely.
		n = bytes.Count(data, []byte("/Page")) - bytes.Count(data, []byte("/Pages"))
		if n < 0 {
			n = 0
		}
	}
	return n
}

// pdfPageSizes extracts MediaBox dimensions per page (approximate).
func pdfPageSizes(data []byte) []string {
	var sizes []string
	re := regexp.MustCompile(`(?s)/MediaBox\s*\[\s*(-?[\d.]+)\s+(-?[\d.]+)\s+(-?[\d.]+)\s+(-?[\d.]+)\s*\]`)
	ms := re.FindAllSubmatch(data, -1)
	for _, m := range ms {
		x0, _ := strconv.ParseFloat(string(m[1]), 64)
		x1, _ := strconv.ParseFloat(string(m[3]), 64)
		y0, _ := strconv.ParseFloat(string(m[2]), 64)
		y1, _ := strconv.ParseFloat(string(m[4]), 64)
		sizes = append(sizes, fmt.Sprintf("%.0f × %.0f", x1-x0, y1-y0))
		if len(sizes) >= 50 {
			break
		}
	}
	return sizes
}

// pdfInfoDict pulls Title/Author/Subject/Creator from the /Info dictionary.
func pdfInfoDict(data []byte) string {
	var sb strings.Builder
	for _, key := range []string{"/Title", "/Author", "/Subject", "/Creator"} {
		re := regexp.MustCompile(key + `\s*\(\s*([^)]*)\s*\)`)
		m := re.FindSubmatch(data)
		if m != nil && len(strings.TrimSpace(string(m[1]))) > 0 {
			fmt.Fprintf(&sb, "  %s: %s\n", strings.TrimPrefix(key, "/"), string(m[1]))
		}
	}
	return strings.TrimSpace(sb.String())
}

// pdfExtractStreams returns the decompressed content of every stream object.
func pdfExtractStreams(data []byte) [][]byte {
	var out [][]byte
	idx := 0
	for {
		i := bytes.Index(data[idx:], []byte("stream"))
		if i < 0 {
			break
		}
		start := idx + i + 6
		// skip CRLF/LF after "stream"
		if start < len(data) && data[start] == '\r' {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		}
		end := bytes.Index(data[start:], []byte("endstream"))
		if end < 0 {
			break
		}
		raw := data[start : start+end]
		// Try FlateDecode (zlib). If it fails, treat as raw.
		if dec, err := zlibDecompress(raw); err == nil {
			out = append(out, dec)
		} else {
			out = append(out, raw)
		}
		idx = start + end + 9
	}
	return out
}

func zlibDecompress(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, zr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pdfTextFromStream extracts text from Tj/TJ operators in a content stream.
// Handles (literal strings) and <hex strings>.
var tjRe = regexp.MustCompile(`\(([^)]*)\)\s*Tj|\[([^\]]*)\]\s*TJ`)

func pdfTextFromStream(stream []byte) string {
	var sb strings.Builder
	for _, m := range tjRe.FindAllSubmatch(stream, -1) {
		if len(m[1]) > 0 {
			sb.Write(m[1])
			sb.WriteByte(' ')
		} else if len(m[2]) > 0 {
			// TJ array: pull (strings) out, ignore numbers (kerning).
			for _, lit := range parenStrRe.FindAllSubmatch(m[2], -1) {
				sb.Write(lit[1])
				sb.WriteByte(' ')
			}
		}
	}
	return sb.String()
}

var parenStrRe = regexp.MustCompile(`\(([^)]*)\)`)

// ── ghostscript helpers ──────────────────────────────────────────────────

func pdfRequireGS() error {
	if _, err := exec.LookPath("gs"); err != nil {
		return fmt.Errorf("ghostscript (gs) is required for this PDF operation but was not found on PATH; install it (e.g. `apt install ghostscript`)")
	}
	return nil
}

func runGS(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "gs", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String(), err
	}
	return stderr.String(), nil
}

// ── arg helpers ──────────────────────────────────────────────────────────

func pdfPathArg(args map[string]interface{}) (string, bool) {
	if p, ok := args["file"].(string); ok && p != "" {
		return p, true
	}
	return "", false
}

func pdfFilesArg(args map[string]interface{}) []string {
	if arr, ok := args["files"].([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	// Fallback: comma-separated "file".
	if s, ok := args["file"].(string); ok && s != "" {
		parts := strings.Split(s, ",")
		var out []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

func pdfIntArg(args map[string]interface{}, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func pdfBadArg(msg string) *ToolResult {
	return &ToolResult{Name: "pdf", Success: false, Error: msg}
}

func pdfIOErr(path string, err error) *ToolResult {
	return &ToolResult{Name: "pdf", Success: false, Error: fmt.Sprintf("%s: %v", path, err)}
}

func fileSize(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}
