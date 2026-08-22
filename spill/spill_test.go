package spill

import (
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestLargeResultIsRecoverable is the regression.
//
// Tool observations were capped with strutil.Truncate, which appends "..." and
// discards the remainder. Reading a 200 KB file gave the model 4 KB and
// destroyed the other 196 KB — no handle, no second page, no way to ask for
// the part that mattered.
func TestLargeResultIsRecoverable(t *testing.T) {
	s := newStore(t)

	// A distinctive marker deep inside, past any truncation point.
	body := strings.Repeat("filler line\n", 20_000)
	needle := "THE-ANSWER-IS-HERE"
	content := body + needle + "\n" + body

	obs := Observe(s, "read_file", content, DefaultThreshold)

	if len(obs) >= len(content) {
		t.Fatalf("observation was not reduced: %d vs %d bytes", len(obs), len(content))
	}
	id := extractID(t, obs)

	// The whole thing must still be reachable, which truncation could not do.
	var got strings.Builder
	for off := 0; off < len(content); off += 8000 {
		chunk, err := s.Get(id, off, 8000)
		if err != nil {
			t.Fatalf("Get(%d): %v", off, err)
		}
		if chunk == "" {
			break
		}
		got.WriteString(chunk)
	}
	if got.String() != content {
		t.Errorf("paged content differs from the original (%d vs %d bytes)", got.Len(), len(content))
	}
	if !strings.Contains(got.String(), needle) {
		t.Error("the marker past the truncation point was not recoverable — " +
			"this is the information loss the package exists to stop")
	}
}

// TestPreviewShowsBothEnds — a test run puts its summary last and a stack
// trace puts the cause first. Head-only truncation loses whichever end matters.
func TestPreviewShowsBothEnds(t *testing.T) {
	s := newStore(t)
	content := "FIRST-LINE-MARKER\n" + strings.Repeat("x", 50_000) + "\nLAST-LINE-MARKER"

	obs := Observe(s, "terminal", content, DefaultThreshold)

	if !strings.Contains(obs, "FIRST-LINE-MARKER") {
		t.Error("preview dropped the head")
	}
	if !strings.Contains(obs, "LAST-LINE-MARKER") {
		t.Error("preview dropped the tail — a test summary lives there")
	}
}

// TestPreviewTellsTheModelHowToContinue — an offloaded result the model cannot
// discover how to read is exactly as lost as a truncated one.
func TestPreviewCarriesRetrievalInstructions(t *testing.T) {
	s := newStore(t)
	content := strings.Repeat("y", 40_000)

	obs := Observe(s, "search_files", content, DefaultThreshold)

	for _, want := range []string{"read_result", "offset", "40000"} {
		if !strings.Contains(obs, want) {
			t.Errorf("preview does not mention %q:\n%s", want, obs[:min(400, len(obs))])
		}
	}
}

// TestSmallResultsPassThroughUntouched — the common case must stay free.
func TestSmallResultsPassThroughUntouched(t *testing.T) {
	s := newStore(t)
	for _, content := range []string{"", "ok", strings.Repeat("z", DefaultThreshold)} {
		if got := Observe(s, "t", content, DefaultThreshold); got != content {
			t.Errorf("a %d-byte result was altered", len(content))
		}
	}
}

// TestContentAddressedSoRepeatsAreFree — an agent loop reads the same file on
// several iterations; that must not write it several times.
func TestContentAddressedSoRepeatsAreFree(t *testing.T) {
	s := newStore(t)
	content := strings.Repeat("q", 30_000)

	a, err := s.Put("read_file", content)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Put("read_file", content)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Errorf("identical content produced two ids: %s vs %s", a.ID, b.ID)
	}
	if c, _ := s.Put("read_file", content+"!"); c.ID == a.ID {
		t.Error("different content collided onto one id")
	}
}

// TestGetRejectsPathsItDidNotIssue — ids are hashes this package produces, but
// a store that opens whatever it is handed is a file-read primitive.
func TestGetRejectsPathsItDidNotIssue(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{
		"../../../../etc/passwd", "..", "/etc/passwd", "", "zzzz", strings.Repeat("a", 17),
	} {
		if _, err := s.Get(bad, 0, 100); err == nil {
			t.Errorf("Get(%q) was accepted", bad)
		}
	}
}

func TestGetPastTheEndIsEmptyNotAnError(t *testing.T) {
	s := newStore(t)
	ref, err := s.Put("t", strings.Repeat("k", 20_000))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ref.ID, 999_999, 100)
	if err != nil {
		t.Errorf("paging off the end errored: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty, got %d bytes", len(got))
	}
}

// TestNilStoreDegradesToTruncation — spilling unavailable must not drop the
// tool call, and the message must say the bytes are gone rather than implying
// with "..." that the output merely ended.
func TestNilStoreDegradesToTruncation(t *testing.T) {
	content := strings.Repeat("w", 50_000)
	got := Observe(nil, "t", content, DefaultThreshold)

	if len(got) >= len(content) {
		t.Error("nil store did not reduce the observation")
	}
	if !strings.Contains(got, "cannot be retrieved") {
		t.Errorf("fallback does not say the bytes were lost: %q", got[len(got)-120:])
	}
}

func extractID(t *testing.T, obs string) string {
	t.Helper()
	const marker = "[result "
	i := strings.Index(obs, marker)
	if i < 0 {
		t.Fatalf("no result header in observation: %q", obs[:min(200, len(obs))])
	}
	rest := obs[i+len(marker):]
	j := strings.IndexAny(rest, " ")
	if j < 0 {
		t.Fatal("malformed result header")
	}
	return rest[:j]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
