package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

// This is the code that tells the user what the agent did to their files. A
// bug here does not crash anything — it misreports a change, which is worse:
// the user approves or moves on based on what this prints.

func TestSplitLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty is no lines, not one blank", "", nil},
		{"single line, no newline", "a", []string{"a"}},
		{"trailing newline does not add a blank", "a\n", []string{"a"}},
		{"interior blank lines are kept", "a\n\nb", []string{"a", "", "b"}},
		{"crlf is normalised", "a\r\nb\r\n", []string{"a", "b"}},
		{"a lone newline is one empty line", "\n", []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLines(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLineDiffShowsOnlyWhatChanged. The whole point of stripping the common
// prefix and suffix is that a one-line edit in a long file reads as a one-line
// edit; printing the entire file back would bury it.
func TestLineDiffShowsOnlyWhatChanged(t *testing.T) {
	before := "a\nb\nc\nd\ne"
	after := "a\nb\nCHANGED\nd\ne"

	got := lineDiff(before, after, 100)

	if !strings.Contains(got, "CHANGED") {
		t.Errorf("diff does not show the new line:\n%s", got)
	}
	if !strings.Contains(got, "c") {
		t.Errorf("diff does not show the removed line:\n%s", got)
	}
	// Unchanged extremities must not be reprinted as changes.
	for _, unchanged := range []string{"-a", "+a", "-e", "+e"} {
		if strings.Contains(got, unchanged) {
			t.Errorf("diff marks unchanged line %q as a change:\n%s", unchanged, got)
		}
	}
}

// TestLineDiffMarksDirection. A user reading "+" for a deletion would draw the
// opposite conclusion from the truth.
func TestLineDiffMarksDirection(t *testing.T) {
	added := lineDiff("keep", "keep\nbrand new", 100)
	if !strings.Contains(added, "+") {
		t.Errorf("a pure addition has no + marker:\n%s", added)
	}
	if strings.Contains(added, "-brand new") {
		t.Errorf("an addition is marked as a removal:\n%s", added)
	}

	removed := lineDiff("keep\ngone", "keep", 100)
	if !strings.Contains(removed, "-") {
		t.Errorf("a pure removal has no - marker:\n%s", removed)
	}
	if strings.Contains(removed, "+gone") {
		t.Errorf("a removal is marked as an addition:\n%s", removed)
	}
}

// TestLineDiffOnIdenticalContent — nothing changed should not read as a change.
func TestLineDiffOnIdenticalContent(t *testing.T) {
	got := lineDiff("same\ncontent", "same\ncontent", 100)
	for _, marker := range []string{"-same", "+same", "-content", "+content"} {
		if strings.Contains(got, marker) {
			t.Errorf("identical content produced change marker %q:\n%s", marker, got)
		}
	}
}

// TestTruncateDiffLinesSaysHowMuchItHid. Silent truncation lets a reader
// believe they saw the whole change.
func TestTruncateDiffLinesSaysHowMuchItHid(t *testing.T) {
	in := strings.Repeat("line\n", 50)

	got := truncateDiffLines(in, 10)
	if strings.Count(got, "\n") > 12 { // 10 kept + the notice
		t.Errorf("truncation kept too much:\n%s", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncated output does not say so:\n%s", got)
	}

	// Under the limit, nothing is touched and no notice is invented.
	short := "a\nb\nc"
	if got := truncateDiffLines(short, 10); got != short {
		t.Errorf("short input was altered: %q", got)
	}
	// A non-positive limit means no limit.
	if got := truncateDiffLines(in, 0); got != in {
		t.Error("maxLines=0 truncated anyway")
	}
}

func TestChangeKindLabel(t *testing.T) {
	kinds := map[core.ChangeKind]string{
		core.ChangeFileCreate: "created",
		core.ChangeFileModify: "modified",
		core.ChangeFileDelete: "deleted",
		core.ChangeCommand:    "ran",
		core.ChangeGit:        "git",
	}
	seenIcons := map[string]core.ChangeKind{}
	for kind, wantLabel := range kinds {
		icon, label := changeKindLabel(core.Change{Kind: kind})
		if label != wantLabel {
			t.Errorf("%v label = %q, want %q", kind, label, wantLabel)
		}
		if icon == "" {
			t.Errorf("%v has no icon", kind)
		}
		if prev, dup := seenIcons[icon]; dup {
			t.Errorf("%v and %v share the icon %q, so they are indistinguishable", kind, prev, icon)
		}
		seenIcons[icon] = kind
	}

	// An unknown kind must still say something rather than render blank.
	icon, label := changeKindLabel(core.Change{Kind: core.ChangeKind("invented")})
	if icon == "" || label == "" {
		t.Errorf("unknown kind rendered blank: icon=%q label=%q", icon, label)
	}
}

// TestPrintChangesNamesEveryFile. A change the summary omits is a change the
// user never reviews.
func TestPrintChangesNamesEveryFile(t *testing.T) {
	changes := []core.Change{
		{Kind: core.ChangeFileCreate, Path: "alpha.go", After: "package alpha\n"},
		{Kind: core.ChangeFileModify, Path: "beta.go", Before: "old\n", After: "new\n"},
		{Kind: core.ChangeFileDelete, Path: "gamma.go", Before: "gone\n"},
	}

	var buf bytes.Buffer
	PrintChanges(&buf, changes)
	out := buf.String()

	for _, path := range []string{"alpha.go", "beta.go", "gamma.go"} {
		if !strings.Contains(out, path) {
			t.Errorf("%q is missing from the change summary:\n%s", path, out)
		}
	}
}

// TestPrintChangesOnNothing. An empty run must not print a summary implying
// work happened.
func TestPrintChangesOnNothing(t *testing.T) {
	var buf bytes.Buffer
	PrintChanges(&buf, nil)
	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("no changes produced output:\n%s", buf.String())
	}
}

func TestCountFileChangesIgnoresCommands(t *testing.T) {
	changes := []core.Change{
		{Kind: core.ChangeFileCreate, Path: "a.go"},
		{Kind: core.ChangeCommand, Path: "go test"},
		{Kind: core.ChangeFileModify, Path: "b.go"},
		{Kind: core.ChangeGit, Path: "commit"},
	}
	if got := countFileChanges(changes); got != 2 {
		t.Errorf("countFileChanges = %d, want 2 (commands and git are not file edits)", got)
	}
}

// TestRenderChangeRespectsTheLineCap. The cap exists so one huge rewrite does
// not push every other change off the screen.
func TestRenderChangeRespectsTheLineCap(t *testing.T) {
	big := strings.Repeat("x\n", 500)

	var buf bytes.Buffer
	renderChange(&buf, core.Change{
		Kind: core.ChangeFileModify, Path: "huge.go", Before: "", After: big,
	}, 5)

	if n := strings.Count(buf.String(), "\n"); n > 40 {
		t.Errorf("a 500-line rewrite rendered %d lines despite a cap of 5", n)
	}
	if !strings.Contains(buf.String(), "huge.go") {
		t.Error("the capped render lost the filename")
	}
}
