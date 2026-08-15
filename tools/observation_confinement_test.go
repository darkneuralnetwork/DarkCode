package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileObservationIsConfinedToWorkspace pins the fix for a go/path-injection
// CodeQL flagged at tools/registry.go — the read inside noteFileObservation
// took the model-supplied "path" argument and read it with no confinement.
//
// The tool call itself is not the problem: the permission gate decides what the
// agent may look at, and confining reads would break an approved look at a
// config in $HOME. The problem is what happens to the bytes. This observation
// is fed to the file observer, which writes them into the knowledge graph
// (app_wireup.go wires it to memory.ObserveFile), so a one-off approved read of
// a file outside the project became a durable belief about that file.
//
// Against the unfixed registry.go this test fails on the first case: the
// observer is called with the contents of a file the workspace has no business
// remembering.
func TestFileObservationIsConfinedToWorkspace(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()

	inWS := filepath.Join(ws, "owned.txt")
	if err := os.WriteFile(inWS, []byte("belongs to the project"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stands in for ~/.ssh/config: readable, real, and none of the graph's
	// business. A path an unprivileged process cannot read would make this test
	// pass for the wrong reason, which is the trap TestConfineWrite documents.
	elsewhere := filepath.Join(outside, "secrets.txt")
	if err := os.WriteFile(elsewhere, []byte("token=hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}

	var observed []string
	r := NewRegistry()
	r.SetFileObserver(func(path, content string) {
		observed = append(observed, path+"\x00"+content)
	})

	ok := &ToolResult{Success: true}
	ctx := withWorkspace(ws)

	r.noteFileObservation(ctx, "read_file", map[string]interface{}{"path": elsewhere}, ok)
	for _, o := range observed {
		if strings.Contains(o, "hunter2") {
			t.Fatalf("a file outside the workspace was observed into the graph: %q", o)
		}
	}
	if len(observed) != 0 {
		t.Fatalf("expected no observation for an out-of-workspace path, got %d", len(observed))
	}

	// The guard must not break the case it exists to allow.
	r.noteFileObservation(ctx, "read_file", map[string]interface{}{"path": inWS}, ok)
	if len(observed) != 1 || !strings.Contains(observed[0], "belongs to the project") {
		t.Fatalf("an in-workspace read should still be observed, got %v", observed)
	}

	// Fail closed: no workspace on the request means no confinement is
	// possible, so nothing is recorded. Same posture as confineWrite.
	observed = nil
	r.noteFileObservation(context.Background(), "read_file", map[string]interface{}{"path": inWS}, ok)
	if len(observed) != 0 {
		t.Fatalf("a request with no workspace must not record an observation, got %v", observed)
	}

	// A symlink inside the workspace pointing outside it. The first version of
	// this guard checked the symlink-resolved path and then read the string it
	// was handed, so this case passed the check under its own name and was
	// opened under the target's — the containment was decorative for exactly
	// the input designed to defeat it.
	observed = nil
	link := filepath.Join(ws, "innocent.txt")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r.noteFileObservation(ctx, "read_file", map[string]interface{}{"path": link}, ok)
	for _, o := range observed {
		if strings.Contains(o, "hunter2") {
			t.Fatalf("a symlink escaped confinement and was observed: %q", o)
		}
	}
}
