package attach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Attachments are how a user hands the agent a file, and everything resolved
// here ends up in the prompt sent to the provider. The package had no tests.

func TestParseRefsExtractsAttachmentsAndCleansTheQuery(t *testing.T) {
	query, atts := ParseRefs("summarize @File:main.go this please")

	if len(atts) != 1 {
		t.Fatalf("parsed %d attachments, want 1: %+v", len(atts), atts)
	}
	if atts[0].Path != "main.go" {
		t.Errorf("path = %q, want main.go", atts[0].Path)
	}
	if strings.Contains(query, "@File") || strings.Contains(query, "main.go") {
		t.Errorf("the reference is still in the query: %q", query)
	}
	if query != "summarize this please" {
		t.Errorf("query = %q, want the whitespace collapsed", query)
	}
}

// TestParseRefsLeavesOrdinaryAtSignsAlone. Email addresses, decorators and
// handles all contain '@'; treating them as references would silently delete
// them from the question.
func TestParseRefsLeavesOrdinaryAtSignsAlone(t *testing.T) {
	for _, in := range []string{
		"email me at someone@example.com",
		"the @decorator syntax",
		"@",
		"@:",
		"@Unknown:thing",
		"cost @ 5 dollars",
	} {
		query, atts := ParseRefs(in)
		if len(atts) != 0 {
			t.Errorf("%q produced %d attachments: %+v", in, len(atts), atts)
		}
		if !strings.Contains(query, "@") {
			t.Errorf("%q lost its @ from the query: %q", in, query)
		}
	}
}

func TestParseRefsHandlesSeveralAndIsCaseInsensitive(t *testing.T) {
	query, atts := ParseRefs("@file:a.go and @DIR:pkg and @Text:hello compare them")

	if len(atts) != 3 {
		t.Fatalf("parsed %d attachments, want 3: %+v", len(atts), atts)
	}
	kinds := map[string]bool{}
	for _, a := range atts {
		kinds[strings.ToLower(a.Type)] = true
	}
	for _, want := range []string{TypeFile, TypeDirectory, TypeText} {
		if !kinds[strings.ToLower(want)] {
			t.Errorf("type %q was not parsed; got %v", want, kinds)
		}
	}
	if !strings.Contains(query, "compare them") {
		t.Errorf("query lost its text: %q", query)
	}
}

func TestResolveReadsAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, results := Resolve([]Attachment{{Type: TypeFile, Path: "hello.go"}}, dir)

	if len(results) != 1 || !results[0].OK {
		t.Fatalf("resolve failed: %+v", results)
	}
	if !strings.Contains(body, "package main") {
		t.Errorf("file contents missing from the rendered block:\n%s", body)
	}
	// The extension becomes a fence hint so the model sees it as code.
	if !strings.Contains(body, "```go") {
		t.Errorf("no language hint on the fence:\n%s", body)
	}
}

// TestResolveReportsAMissingFileInsteadOfFailing. One bad attachment must not
// discard the others, and the model needs to see that the file was not read
// rather than assume an empty file.
func TestResolveReportsAMissingFileInsteadOfFailing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, results := Resolve([]Attachment{
		{Type: TypeFile, Path: "absent.txt"},
		{Type: TypeFile, Path: "real.txt"},
	}, dir)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].OK {
		t.Error("a missing file reported OK")
	}
	if !results[1].OK {
		t.Error("the good attachment was dropped because a sibling failed")
	}
	if !strings.Contains(body, "could not attach") {
		t.Errorf("the failure is not visible in the prompt:\n%s", body)
	}
	if !strings.Contains(body, "here") {
		t.Errorf("the readable file is missing from the prompt:\n%s", body)
	}
}

// TestBinaryFilesAreDescribedNotDumped. Pasting a binary into the prompt
// wastes the context window on bytes the model cannot use, and can break the
// request encoding outright.
func TestBinaryFilesAreDescribedNotDumped(t *testing.T) {
	dir := t.TempDir()
	blob := []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02, 0x00}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	body, results := Resolve([]Attachment{{Type: TypeFile, Path: "a.bin"}}, dir)

	if !results[0].OK {
		t.Fatalf("binary attachment errored: %+v", results[0])
	}
	if !strings.Contains(body, "binary file") {
		t.Errorf("a binary was not labelled as one:\n%s", body)
	}
	if strings.Contains(body, "\x00") {
		t.Error("raw NUL bytes reached the prompt")
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("plain text\nwith newlines\n")) {
		t.Error("plain text detected as binary")
	}
	if !isBinary([]byte{'a', 0x00, 'b'}) {
		t.Error("a NUL byte was not detected as binary")
	}
	if isBinary(nil) {
		t.Error("empty input detected as binary")
	}
	// UTF-8 is text, not binary.
	if isBinary([]byte("héllo — ünïcode ✓")) {
		t.Error("UTF-8 detected as binary")
	}
}

// TestLargeFilesAreTruncatedAndSaySo. Silently cutting a file lets the model
// answer confidently about content it never received.
func TestLargeFilesAreTruncatedAndSaySo(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxFileBytes*2)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	body, results := Resolve([]Attachment{{Type: TypeFile, Path: "big.txt"}}, dir)

	if !results[0].OK {
		t.Fatalf("large file errored: %+v", results[0])
	}
	if len(body) > maxFileBytes*3/2 {
		t.Errorf("a %d-byte file produced %d bytes of prompt", len(big), len(body))
	}
	if !strings.Contains(body, "truncated") {
		t.Error("the truncation is not announced, so the model cannot know it saw a fragment")
	}
}

// TestResolveOnNothing. No attachments must add nothing to the prompt — an
// empty "## Attachments" heading is context spent on a section with no content.
func TestResolveOnNothing(t *testing.T) {
	body, results := Resolve(nil, t.TempDir())
	if body != "" {
		t.Errorf("no attachments produced prompt text: %q", body)
	}
	if len(results) != 0 {
		t.Errorf("no attachments produced %d results", len(results))
	}
}

func TestResolvePathStaysRelativeToTheWorkspace(t *testing.T) {
	ws := t.TempDir()

	if got := resolvePath("sub/file.go", ws); got != filepath.Join(ws, "sub/file.go") {
		t.Errorf("relative path resolved to %q", got)
	}
	// An absolute path is taken at face value — the user naming a path outside
	// the workspace is asking for that file on purpose.
	abs := filepath.Join(t.TempDir(), "elsewhere.txt")
	if got := resolvePath(abs, ws); got != abs {
		t.Errorf("absolute path resolved to %q, want %q", got, abs)
	}
	// An empty path is the workspace itself, not the filesystem root.
	if got := resolvePath("", ws); got != ws {
		t.Errorf("empty path resolved to %q, want the workspace", got)
	}
}

// TestReadURLAttachmentBlocksPrivateAddresses covers the SSRF guard on a
// @url: attachment — a URL a user (or, once attached, an agent reasoning
// over the conversation) supplies, the exact "URL a model or web page
// chose" category safeurl.SafeClient's own doc comment names. This used to
// pair IsSafeFetchURL's early check with safeurl.EgressClient, which has no
// SSRF restriction outside air-gap mode — closing only the check-time gap,
// not the dial-time one a DNS-rebinding attack needs. Now uses SafeClient,
// matching what safeurl.go documents as correct for this call shape.
func TestReadURLAttachmentBlocksPrivateAddresses(t *testing.T) {
	for _, url := range []string{
		"http://localhost/",
		"http://metadata.google.internal/", // cloud metadata endpoint, by name
		"http://metadata/",                 // short form of the same
	} {
		if _, _, err := readURLAttachment(url); err == nil {
			t.Errorf("readURLAttachment(%q) succeeded, want it blocked by the SSRF guard", url)
		}
	}
}
