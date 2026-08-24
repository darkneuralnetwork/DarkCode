package server

import (
	"testing"

	"github.com/darkcode/infra/config"
	"github.com/darkcode/memory/memory"
)

// TestEnsureWorkspaceIndexedWiresAutoIngest is the regression test for Phase
// 7's CLI ingestion fix: a surface with no HTTP requests at all (CLI) must
// still get the same push-based semantic ingestion a GUI/server session
// gets automatically the moment any handler touches a workspace. Before
// EnsureWorkspaceIndexed existed, nothing on the CLI path ever called the
// unexported projectIndex, so IngestInBackground()'s "on by default" setting
// was silently a no-op for every CLI-only session.
func TestEnsureWorkspaceIndexedWiresAutoIngest(t *testing.T) {
	mem, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("memory.NewSystem: %v", err)
	}
	t.Cleanup(mem.Shutdown)

	cfg := &config.Config{AutoIngest: true}
	s := NewServer(cfg, nil, mem, nil, nil, nil, nil, nil, nil)

	workspace := t.TempDir()
	s.EnsureWorkspaceIndexed(workspace)

	s.indexMu.Lock()
	_, indexed := s.indexes[workspace]
	s.indexMu.Unlock()
	if !indexed {
		t.Fatal("EnsureWorkspaceIndexed did not register a ProjectIndex for the workspace")
	}
	if _, ok := s.ingesters[workspace]; !ok {
		t.Error("EnsureWorkspaceIndexed did not chain auto-ingest onto the workspace watcher — AutoIngest is on, so it should have")
	}
}

// TestEnsureWorkspaceIndexedIsIdempotent proves a second call (the CLI's
// startup call racing a GUI session that already indexed the same
// workspace) reuses the existing index instead of building a second one —
// same map-reuse guarantee projectIndex's own doc comment already claims,
// exercised through the exported entry point CLI actually calls.
func TestEnsureWorkspaceIndexedIsIdempotent(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(cfg, nil, nil, nil, nil, nil, nil, nil, nil)

	workspace := t.TempDir()
	s.EnsureWorkspaceIndexed(workspace)
	s.indexMu.Lock()
	first := s.indexes[workspace]
	s.indexMu.Unlock()

	s.EnsureWorkspaceIndexed(workspace)
	s.indexMu.Lock()
	second := s.indexes[workspace]
	s.indexMu.Unlock()

	if first != second {
		t.Error("a second EnsureWorkspaceIndexed call for the same workspace built a new index instead of reusing it")
	}
}
