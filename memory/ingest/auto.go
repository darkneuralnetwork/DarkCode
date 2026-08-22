package ingest

// auto.go — keeping the retrieval index fed without being asked.
//
// Ingestion was reachable three ways — a tool the model can call, a CLI
// command, an HTTP endpoint — and all three are deliberate acts. So semantic
// memory held whatever somebody had remembered to feed it, which for most
// installs is nothing. The knowledge graph filled itself from AST sync and task
// outcomes; the vector index next to it stayed empty. Retrieval was then
// blamed for missing things it had never been shown.
//
// The cost of fixing that is real and worth stating plainly, because it is paid
// per chunk: every chunk stored is one embedding call. A full pass over a
// medium repository is thousands of them. That is free on a local embedder and
// billable on a hosted one, and it is the reason this is incremental rather
// than a rescan.
//
// So the unit of work is a file whose CONTENT changed. A manifest of content
// hashes decides that, which means the steady state — an editor saving the same
// file, a restart, a watcher tick on an untouched tree — costs a hash and no
// embedding at all. The first pass is the expensive one and it happens once.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/infra/observability"
)

// manifestName is where the per-file content hashes live, inside the memory
// directory rather than the workspace: it is DarkCode's bookkeeping, and
// writing it into the user's repository would be a file they did not ask for.
const manifestName = "ingest_manifest.json"

// autoIngestMaxFiles bounds a single pass so pointing DarkCode at a very large
// tree degrades into "indexed the first N files" rather than into an
// unbounded embedding bill. Files are visited in a stable order, so the same
// prefix is indexed each pass and a raised limit extends it rather than
// reshuffling it.
const autoIngestMaxFiles = 4000

// Auto keeps one workspace's ingested knowledge current.
type Auto struct {
	in   *Ingester
	root string
	path string // manifest location

	mu      sync.Mutex
	hashes  map[string]string // absolute file path → content hash
	chunkNo map[string]int    // absolute file path → chunks stored last time
	running bool
}

// manifest is the on-disk shape.
type manifest struct {
	Hashes map[string]string `json:"hashes"`
	Chunks map[string]int    `json:"chunks"`
}

// NewAuto builds an incremental ingester for root. memDir is where the
// manifest is persisted so a restart does not re-embed a tree that has not
// changed; pass "" to keep it in memory (every process start re-indexes).
func NewAuto(in *Ingester, root, memDir string) *Auto {
	a := &Auto{
		in:      in,
		root:    root,
		hashes:  map[string]string{},
		chunkNo: map[string]int{},
	}
	if memDir != "" {
		a.path = filepath.Join(memDir, manifestName)
		a.load()
	}
	return a
}

// Sync brings the index up to date with the workspace and reports what it did.
// Only files whose content hash changed are read, chunked and embedded.
func (a *Auto) Sync(ctx context.Context) Stats {
	var st Stats

	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return st // a pass is already in flight; overlapping them buys nothing
	}
	a.running = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()

	seen := map[string]bool{}
	var candidates []string
	_ = filepath.WalkDir(a.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !ingestibleExt(path) {
			return nil
		}
		candidates = append(candidates, path)
		return ctx.Err()
	})
	// Stable order so a bounded pass covers the same prefix every time.
	sort.Strings(candidates)
	if len(candidates) > autoIngestMaxFiles {
		candidates = candidates[:autoIngestMaxFiles]
	}

	for _, path := range candidates {
		if ctx.Err() != nil {
			break
		}
		seen[path] = true

		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 || info.Size() > maxFileBytes {
			st.Skipped++
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			st.addErr("%s: %v", path, err)
			continue
		}
		if isBinary(data) {
			st.Skipped++
			continue
		}

		sum := hashOf(data)
		a.mu.Lock()
		unchanged := a.hashes[path] == sum
		prevChunks := a.chunkNo[path]
		a.mu.Unlock()
		if unchanged {
			st.Skipped++
			continue // the expensive path skipped entirely: no chunking, no embedding
		}

		category := "doc"
		if isCodeExt(path) {
			category = "code"
		}
		stored := a.in.storeChunks(path, category, string(data), &st)
		if stored > 0 {
			st.Sources++
		}
		// A file that shrank leaves chunks behind. Without this they are never
		// reachable by any future write (the key encodes the index) and would
		// be retrieved forever as text that no longer exists in the file.
		a.pruneChunks(path, category, stored, prevChunks)

		a.mu.Lock()
		a.hashes[path] = sum
		a.chunkNo[path] = stored
		a.mu.Unlock()
	}

	// Files that have been deleted since the last pass.
	a.forgetMissing(seen, &st)
	a.save()
	return st
}

// SyncFiles re-ingests a specific set of paths — the watcher's incremental
// path. Same hash guard, so a save that did not change the bytes costs nothing.
func (a *Auto) SyncFiles(ctx context.Context, paths []string) Stats {
	var st Stats
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		if !ingestibleExt(path) || !strings.HasPrefix(path, a.root) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			// Gone: drop what we stored for it rather than keep serving it.
			a.forgetOne(path, &st)
			continue
		}
		if info.Size() == 0 || info.Size() > maxFileBytes {
			st.Skipped++
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || isBinary(data) {
			st.Skipped++
			continue
		}
		sum := hashOf(data)
		a.mu.Lock()
		unchanged := a.hashes[path] == sum
		prevChunks := a.chunkNo[path]
		a.mu.Unlock()
		if unchanged {
			st.Skipped++
			continue
		}
		category := "doc"
		if isCodeExt(path) {
			category = "code"
		}
		stored := a.in.storeChunks(path, category, string(data), &st)
		if stored > 0 {
			st.Sources++
		}
		a.pruneChunks(path, category, stored, prevChunks)
		a.mu.Lock()
		a.hashes[path] = sum
		a.chunkNo[path] = stored
		a.mu.Unlock()
	}
	if st.Sources > 0 || st.Chunks > 0 {
		a.save()
	}
	return st
}

// pruneChunks removes chunk entries left over from a longer previous version.
func (a *Auto) pruneChunks(path, category string, now, before int) {
	for i := now; i < before; i++ {
		_ = a.in.mem.SemanticRemove(fmt.Sprintf("ingest:%s:%s#%d", category, path, i))
	}
}

// forgetMissing drops every file the manifest knows about that the walk did
// not see. A deleted file whose chunks stay indexed is a source that answers
// questions about code that is gone.
func (a *Auto) forgetMissing(seen map[string]bool, st *Stats) {
	a.mu.Lock()
	var gone []string
	for path := range a.hashes {
		if !seen[path] {
			gone = append(gone, path)
		}
	}
	a.mu.Unlock()
	for _, path := range gone {
		a.forgetOne(path, st)
	}
}

func (a *Auto) forgetOne(path string, st *Stats) {
	a.mu.Lock()
	n := a.chunkNo[path]
	delete(a.hashes, path)
	delete(a.chunkNo, path)
	a.mu.Unlock()
	for _, category := range []string{"code", "doc"} {
		for i := 0; i < n; i++ {
			_ = a.in.mem.SemanticRemove(fmt.Sprintf("ingest:%s:%s#%d", category, path, i))
		}
	}
	if n > 0 {
		st.Skipped++
	}
}

// Start runs one pass now and then re-syncs whenever the watcher reports
// changes. It returns immediately.
//
// Guarded, because this parses arbitrary files off the request path: an
// unrecovered panic on a malformed input here would take the process down
// rather than fail the indexing.
func (a *Auto) Start(ctx context.Context) {
	observability.Go("workspace-ingest", func() {
		st := a.Sync(ctx)
		observability.Log().Info("workspace indexed for retrieval", map[string]interface{}{
			"root": a.root, "files": st.Sources, "chunks": st.Chunks, "unchanged": st.Skipped,
		})
	})
}

// OnChanged is the watcher hook: re-ingest just what changed.
func (a *Auto) OnChanged(paths []string) {
	observability.Go("workspace-ingest-delta", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		a.SyncFiles(ctx, paths)
	})
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func (a *Auto) load() {
	blob, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	var m manifest
	if json.Unmarshal(blob, &m) != nil {
		return // a corrupt manifest costs one re-index, not a failure
	}
	if m.Hashes != nil {
		a.hashes = m.Hashes
	}
	if m.Chunks != nil {
		a.chunkNo = m.Chunks
	}
}

func (a *Auto) save() {
	if a.path == "" {
		return
	}
	a.mu.Lock()
	blob, err := json.Marshal(manifest{Hashes: a.hashes, Chunks: a.chunkNo})
	a.mu.Unlock()
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, blob, 0o600) == nil {
		_ = os.Rename(tmp, a.path)
	}
}
