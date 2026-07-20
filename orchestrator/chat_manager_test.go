package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpectedArtifacts(t *testing.T) {
	cm := NewChatManager()

	web := cm.ExpectedArtifacts("make a small website for my cafe")
	var htmlReq bool
	for _, a := range web {
		if a.Ext == ".html" && a.Required {
			htmlReq = true
		}
	}
	if !htmlReq {
		t.Errorf("a website goal should require an .html artifact, got %+v", web)
	}

	// A pure question implies no artifacts.
	if got := cm.ExpectedArtifacts("what does the router do?"); len(got) != 0 {
		t.Errorf("a question should imply no artifacts, got %+v", got)
	}
}

func TestCheckCompleteness(t *testing.T) {
	cm := NewChatManager()
	dir := t.TempDir()

	// Website goal, empty workspace → incomplete (missing .html).
	done, gaps := cm.CheckCompleteness("build a website", dir)
	if done || len(gaps) == 0 {
		t.Fatalf("empty workspace should be incomplete for a website, got done=%v gaps=%v", done, gaps)
	}

	// Add an .html → the required artifact is now present → complete
	// (.css/.js are optional for a bare "website").
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0644)
	done, gaps = cm.CheckCompleteness("build a website", dir)
	if !done {
		t.Fatalf("website with an .html should be complete, got gaps=%v", gaps)
	}

	// A non-artifact goal is always complete.
	if done, _ := cm.CheckCompleteness("explain how channels work", dir); !done {
		t.Error("a question goal should be considered complete")
	}

	// Ignores vendored dirs.
	sub := filepath.Join(dir, "node_modules", "x")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "dep.js"), []byte("x"), 0644)
	if done, gaps := cm.CheckCompleteness("write a javascript file", t.TempDir()); done {
		t.Errorf("js goal in an empty ws should be incomplete, got gaps=%v", gaps)
	}
}
