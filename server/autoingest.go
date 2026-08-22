package server

// autoingest.go — hanging workspace indexing off the same watcher that keeps
// the code index fresh.
//
// The code index and the retrieval index answer different questions — one
// structural ("where is X defined"), one by similarity ("what looks like this")
// — but they go stale on exactly the same event: a file changed. So they share
// a watcher rather than each running their own poller over the same tree.

import (
	"context"

	"github.com/darkcode/ingest"
	"github.com/darkcode/intelligence"
	"github.com/darkcode/observability"
)

// startAutoIngest begins (or re-attaches) incremental workspace ingestion for
// this workspace, chaining onto the code index's watcher.
//
// Called with s.indexMu held by projectIndex.
func (s *Server) startAutoIngest(workspace string, idx *intelligence.ProjectIndex) {
	s.cfgMu.RLock()
	enabled := s.cfg.IngestInBackground()
	memDir := s.cfg.MemoryDir
	s.cfgMu.RUnlock()

	if !enabled || s.memSystem == nil {
		return
	}
	if _, exists := s.ingesters[workspace]; exists {
		return
	}

	auto := ingest.NewAuto(ingest.New(s.memSystem, s.memSystem.KG()), workspace, memDir)
	if s.ingesters == nil {
		s.ingesters = map[string]*ingest.Auto{}
	}
	s.ingesters[workspace] = auto

	// Chain rather than replace: the code index set this callback first and
	// still needs its half. Dropping it would trade a stale retrieval index
	// for a stale symbol index, which is not a trade.
	if w := idx.Watcher(); w != nil {
		prev := w.OnChange
		w.OnChange = func(changed []string) {
			if prev != nil {
				prev(changed)
			}
			auto.OnChanged(changed)
		}
	}

	observability.Log().Info("indexing workspace for retrieval in the background",
		map[string]interface{}{"workspace": workspace})
	auto.Start(context.Background())
}
