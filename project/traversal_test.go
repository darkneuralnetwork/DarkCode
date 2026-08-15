package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectIDCannotEscapeTheStoreRoot pins the fix for the path traversal
// CodeQL reported as fifteen go/path-injection sinks in store.go.
//
// The ids this package makes are safe — newID is a slug plus hex and slugify's
// charset has no separator. The ids it is GIVEN were never checked, and they
// come from outside: server/chat_handler.go passes req.Project, straight off
// the request body, into Get, GetPlan and GetWorkflow. Every one of those
// reaches dir(), where filepath.Join collapses "..".
//
// Against the unguarded dir() the write below lands outside the root.
func TestProjectIDCannotEscapeTheStoreRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}

	// A relative escape, an absolute path, and a Windows-style separator. The
	// third matters because filepath.Base is separator-aware per platform and a
	// caller may hand us either.
	for _, id := range []string{
		"../../" + filepath.Base(outside),
		filepath.Join(outside, "planted"),
		"..",
		".",
		"",
		"a/../../b",
	} {
		t.Run("id="+id, func(t *testing.T) {
			got := s.dir(id)
			abs, err := filepath.Abs(got)
			if err != nil {
				t.Fatal(err)
			}
			rootAbs, _ := filepath.Abs(root)
			rel, err := filepath.Rel(rootAbs, abs)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("id %q resolved to %q, which is outside the store root %q", id, abs, rootAbs)
			}
			// And it must be a single component: a nested path inside the root
			// is still an id addressing something it did not create.
			if strings.Contains(rel, string(filepath.Separator)) {
				t.Fatalf("id %q resolved to nested path %q inside the root", id, rel)
			}
		})
	}

	// End to end: the write a traversing id would have performed must not
	// appear outside the root. SetContext is the reachable writer.
	victim := filepath.Join(outside, "context.md")
	if err := s.SetContext("../../"+filepath.Base(outside), "planted by a traversing id"); err != nil {
		t.Logf("SetContext returned %v (a refusal is also an acceptable outcome)", err)
	}
	if b, err := os.ReadFile(victim); err == nil && strings.Contains(string(b), "planted") {
		t.Fatalf("a traversing project id wrote outside the store root: %s", victim)
	}

	// The guard must not break a real id.
	p, err := s.Create("My Project", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetContext(p.ID, "legitimate"); err != nil {
		t.Fatalf("a real project id was refused: %v", err)
	}
	got, err := s.GetWithContext(p.ID)
	if err != nil || !strings.Contains(got.Context, "legitimate") {
		t.Fatalf("round trip through a real id broke: %v / %+v", err, got)
	}
}
