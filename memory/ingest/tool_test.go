package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkcode/infra/core"
)

func withWorkspace(ws string) context.Context {
	return context.WithValue(context.Background(), core.WorkspaceKey, ws)
}

// TestIngestToolConfinesFilePaths covers the gap CodeQL found: the
// agent-callable "ingest" tool read whatever file/directory path the model
// named with no workspace confinement at all, then unconditionally persisted
// what it read into semantic memory — unlike read_file, which registry.go's
// noteFileObservation already confines for exactly this "one-off look
// becomes a durable belief" reason. A model tricked (by injected content
// earlier in the same conversation) into calling ingest with a path like
// ~/.ssh/config would have had it embedded into long-term memory.
func TestIngestToolConfinesFilePaths(t *testing.T) {
	mem := newMem(t)
	entry := NewIngestTool(mem, nil, nil)

	ws := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not ingest me"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := entry.Handler(withWorkspace(ws), map[string]interface{}{"source": secret})
	if res.Success {
		t.Errorf("ingest tool read a path outside the workspace: %+v", res)
	}

	// An in-workspace file must still work — a fix that blocks everything is
	// as useless as one that blocks nothing.
	inWs := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(inWs, []byte("Goroutines are lightweight threads."), 0o644); err != nil {
		t.Fatal(err)
	}
	res = entry.Handler(withWorkspace(ws), map[string]interface{}{"source": inWs})
	if !res.Success {
		t.Errorf("ingest tool rejected an ordinary in-workspace file: %+v", res)
	}
}

// TestIngestToolStillAcceptsURLsAndText covers the two source kinds the
// confinement check must not touch — a URL is guarded separately inside
// IngestURL (safeurl.SafeClient), and raw pasted text has no path at all.
func TestIngestToolStillAcceptsURLsAndText(t *testing.T) {
	mem := newMem(t)
	entry := NewIngestTool(mem, nil, nil)
	ws := t.TempDir()

	// No workspace on the context at all — neither source kind should need
	// one, since neither is a filesystem path.
	res := entry.Handler(context.Background(), map[string]interface{}{
		"source": "Goroutines are lightweight threads managed by the Go runtime.",
	})
	if !res.Success {
		t.Errorf("raw text source was rejected: %+v", res)
	}

	res = entry.Handler(withWorkspace(ws), map[string]interface{}{
		"source": "http://metadata.google.internal/", // cloud metadata endpoint, by name
	})
	if res.Success {
		t.Error("a cloud-metadata URL should have been blocked by IngestURL's SSRF guard, not silently ingested")
	}
}
