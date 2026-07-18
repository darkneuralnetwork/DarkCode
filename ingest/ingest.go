// Package ingest builds durable knowledge from external material — documents,
// books, source files, whole repositories, or URLs — so the local model can
// retrieve over it offline (RAG). Each source is loaded, split into overlapping
// chunks, embedded, and stored in semantic memory; source code additionally
// gets its structure indexed into the knowledge graph.
//
// This is the "teach it" half of the offline-first design: chat/tasks
// accumulate knowledge passively, ingestion adds it deliberately.
package ingest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
	"github.com/darkcode/safeurl"
	"github.com/darkcode/tools/deterministic"
)

const (
	// chunkSize / chunkOverlap bound each stored chunk. ~1500 chars keeps a
	// chunk well within a small local model's embedding window while staying
	// large enough to be a meaningful unit of retrieval; the overlap preserves
	// context across a boundary so a fact split across two chunks still matches.
	chunkSize    = 1500
	chunkOverlap = 200
	// maxFileBytes skips oversized files (build artifacts, minified bundles,
	// data dumps) that would bloat memory without being useful knowledge.
	maxFileBytes = 1 << 20 // 1 MiB
	// maxURLBytes caps a fetched page.
	maxURLBytes = 2 << 20 // 2 MiB
)

// Stats summarizes what an ingest run stored.
type Stats struct {
	Sources int      `json:"sources"` // files/URLs/texts ingested
	Chunks  int      `json:"chunks"`  // semantic entries written
	KGNodes int      `json:"kg_nodes"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
}

func (s *Stats) addErr(format string, a ...interface{}) {
	s.Errors = append(s.Errors, fmt.Sprintf(format, a...))
}

// Ingester stores ingested knowledge into semantic memory and (for code) the
// knowledge graph.
type Ingester struct {
	mem *memory.System
	kg  core.KnowledgeGraphStore // optional; enables code-structure indexing
}

// New builds an Ingester over the given memory system. kg may be nil (then code
// is still stored as searchable text chunks, just not graph-indexed).
func New(mem *memory.System, kg core.KnowledgeGraphStore) *Ingester {
	return &Ingester{mem: mem, kg: kg}
}

// Ingest auto-detects the source kind (URL, directory, file, or raw text) and
// dispatches to the appropriate handler.
func (in *Ingester) Ingest(ctx context.Context, source string) (Stats, error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return Stats{}, fmt.Errorf("ingest: empty source")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return in.IngestURL(ctx, s)
	}
	if info, err := os.Stat(s); err == nil {
		if info.IsDir() {
			return in.IngestDir(ctx, s)
		}
		return in.IngestFile(ctx, s)
	}
	// Not a URL or an existing path → treat as raw pasted text.
	return in.IngestText(ctx, "pasted-text", "note", s)
}

// IngestText chunks, embeds, and stores raw text under a source label.
func (in *Ingester) IngestText(ctx context.Context, source, category, text string) (Stats, error) {
	var st Stats
	n := in.storeChunks(source, category, text, &st)
	if n > 0 {
		st.Sources++
	}
	return st, nil
}

// IngestFile reads and ingests a single file.
func (in *Ingester) IngestFile(ctx context.Context, path string) (Stats, error) {
	var st Stats
	in.ingestOneFile(path, &st)
	return st, nil
}

// IngestDir walks a directory/repo and ingests each eligible text/source file.
// When a knowledge graph is configured it also syncs the directory's code
// structure (symbols, imports) into the graph so structural questions ("where
// is X defined") work over the ingested repo.
func (in *Ingester) IngestDir(ctx context.Context, root string) (Stats, error) {
	var st Stats
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			st.addErr("%s: %v", path, err)
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		in.ingestOneFile(path, &st)
		return ctx.Err() // stop early if the context is cancelled
	})
	// Best-effort code-structure indexing for the whole tree.
	if in.kg != nil {
		if kgStats, kgErr := deterministic.SyncWorkspaceKG(ctx, root, in.kg); kgErr == nil {
			st.KGNodes += kgStats.Symbols + kgStats.Files
		} else {
			st.addErr("kg sync: %v", kgErr)
		}
	}
	return st, err
}

// IngestURL fetches a URL through the SSRF-safe client and ingests its text.
func (in *Ingester) IngestURL(ctx context.Context, url string) (Stats, error) {
	var st Stats
	if !safeurl.IsSafeFetchURL(url, false) {
		return st, fmt.Errorf("ingest: refusing unsafe URL (loopback/private/link-local): %s", url)
	}
	client := safeurl.SafeClient(30*time.Second, false)
	req, err := newGetRequest(ctx, url)
	if err != nil {
		return st, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return st, fmt.Errorf("ingest: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return st, fmt.Errorf("ingest: fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxURLBytes))
	if err != nil {
		return st, fmt.Errorf("ingest: read %s: %w", url, err)
	}
	if in.storeChunks(url, "url", string(body), &st) > 0 {
		st.Sources++
	}
	return st, nil
}

// ingestOneFile ingests a single file into st (mutating it), skipping binaries
// and oversized/uninteresting files.
func (in *Ingester) ingestOneFile(path string, st *Stats) {
	info, err := os.Stat(path)
	if err != nil {
		st.addErr("%s: %v", path, err)
		return
	}
	if info.Size() == 0 || info.Size() > maxFileBytes || !ingestibleExt(path) {
		st.Skipped++
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		st.addErr("%s: %v", path, err)
		return
	}
	if isBinary(data) {
		st.Skipped++
		return
	}
	category := "doc"
	if isCodeExt(path) {
		category = "code"
	}
	if in.storeChunks(path, category, string(data), st) > 0 {
		st.Sources++
	}
}

// storeChunks splits content into overlapping chunks and writes each to
// semantic memory. Returns the number of chunks stored.
func (in *Ingester) storeChunks(source, category, content string, st *Stats) int {
	content = strings.TrimSpace(content)
	if content == "" || in.mem == nil {
		return 0
	}
	base := filepath.Base(source)
	chunks := chunk(content, chunkSize, chunkOverlap)
	stored := 0
	for i, c := range chunks {
		key := fmt.Sprintf("ingest:%s:%s#%d", category, source, i)
		tags := []string{category, "ingested", base}
		if err := in.mem.SemanticAdd(key, c, category, tags); err != nil {
			st.addErr("%s#%d: %v", source, i, err)
			continue
		}
		stored++
	}
	st.Chunks += stored
	return stored
}
