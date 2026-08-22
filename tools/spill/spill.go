// Package spill keeps oversized tool results out of the context window without
// throwing them away.
//
// # WHY THIS EXISTS
//
// A tool observation was capped at 4,000 bytes with strutil.Truncate, which
// appends "..." and discards the rest. Reading a 200 KB file gave the model
// 4 KB and destroyed the other 196 KB — not moved it, destroyed it. There was
// no handle, no second page, no way to ask for the part that mattered. The
// model's only recourse was to run the same tool again and get the same first
// 4 KB.
//
// That is the largest avoidable source of both token waste and lost
// information in an agent: tool results, not conversation, are what fill a
// context window, and truncation spends the tokens while losing the answer.
//
// The established fix is to persist the full result and put a preview plus a
// retrievable handle in the context. The model sees the head and the tail —
// where the useful parts of a file listing, a stack trace or a test run
// actually live — plus an exact byte count and an id it can page through with
// the read_result tool.
//
// Nothing is deleted. A spilled result is a file on disk, addressed by the
// hash of its content, and the model can always get the bytes back.
package spill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultThreshold is the observation size above which a result is spilled.
//
// Below this a result costs about a thousand tokens and is cheaper to keep
// inline than to make the model spend a tool call retrieving. Above it, the
// preview plus a handle is strictly better than a truncation: same order of
// tokens, and the remainder stays reachable.
const DefaultThreshold = 4000

// previewHead and previewTail are how much of a spilled result is shown
// inline. Head and tail rather than head alone because the ends are where the
// signal is: a test run puts the summary last, a stack trace puts the cause
// first, a directory listing is uniform and either end will do.
const (
	previewHead = 2000
	previewTail = 1000
)

// Ref describes a spilled result.
type Ref struct {
	ID    string // content address; stable for identical content
	Tool  string // which tool produced it
	Bytes int    // full size, before any preview
	Lines int
}

// Store persists spilled results under a directory.
//
// Content-addressed, so a tool run twice over unchanged input writes once and
// the second call is free. That matters for the common agent loop of reading
// the same file on several iterations.
type Store struct {
	dir string

	mu   sync.RWMutex
	meta map[string]Ref
}

// New opens (creating if needed) a spill store rooted at dir.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("spill: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("spill: cannot create %s: %w", dir, err)
	}
	return &Store{dir: dir, meta: make(map[string]Ref)}, nil
}

// path returns the on-disk location for an id. The id is a hex hash produced
// here, never caller input, but it is still checked: a store that will open
// any path it is handed is a file-read primitive wearing a cache's clothes.
func (s *Store) path(id string) (string, error) {
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", fmt.Errorf("spill: malformed id %q", id)
	}
	return filepath.Join(s.dir, id+".txt"), nil
}

// Put stores content and returns its reference. Storing is best-effort in the
// sense that a disk failure is reported, but callers should treat a failed
// spill as "keep the truncated form" rather than as a failed tool call.
func (s *Store) Put(tool, content string) (Ref, error) {
	sum := sha256.Sum256([]byte(content))
	id := hex.EncodeToString(sum[:8])

	ref := Ref{
		ID:    id,
		Tool:  tool,
		Bytes: len(content),
		Lines: strings.Count(content, "\n") + 1,
	}

	p, err := s.path(id)
	if err != nil {
		return Ref{}, err
	}
	// Content-addressed: identical content is already on disk and identical.
	if _, err := os.Stat(p); err != nil {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return Ref{}, fmt.Errorf("spill: write %s: %w", id, err)
		}
	}

	s.mu.Lock()
	s.meta[id] = ref
	s.mu.Unlock()
	return ref, nil
}

// Get returns a byte range of a spilled result. offset beyond the end returns
// an empty string rather than an error: paging off the end is a normal way to
// discover you have read it all. limit <= 0 means "to the end".
func (s *Store) Get(id string, offset, limit int) (string, error) {
	p, err := s.path(id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("spill: result %s is no longer available: %w", id, err)
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(data) {
		return "", nil
	}
	end := len(data)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return string(data[offset:end]), nil
}

// Stat reports what is known about a spilled result.
func (s *Store) Stat(id string) (Ref, bool) {
	s.mu.RLock()
	ref, ok := s.meta[id]
	s.mu.RUnlock()
	return ref, ok
}

// Observe returns the text to put in the model's context for a tool result.
//
// Small results pass through untouched — the common case, and it must stay
// free. Large ones are written to the store and replaced by a head/tail
// preview carrying the id, the true size, and the instruction needed to fetch
// the rest. When the store is nil or the write fails, the result is truncated
// as before rather than lost entirely: degrading to the old behaviour is
// acceptable, dropping the tool call is not.
func Observe(s *Store, tool, content string, threshold int) string {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if len(content) <= threshold {
		return content
	}
	if s == nil {
		return truncateFallback(content, threshold)
	}
	ref, err := s.Put(tool, content)
	if err != nil {
		return truncateFallback(content, threshold)
	}
	return Render(ref, content)
}

// Render builds the inline preview for an already-stored result.
func Render(ref Ref, content string) string {
	head := previewHead
	tail := previewTail
	if head+tail >= len(content) {
		return content
	}
	omitted := len(content) - head - tail

	var b strings.Builder
	fmt.Fprintf(&b, "[result %s — %d bytes, %d lines. Showing the first %d and last %d bytes; %d omitted.\n"+
		"Read any part with: read_result(id=%q, offset=<byte>, limit=<bytes>)]\n\n",
		ref.ID, ref.Bytes, ref.Lines, head, tail, omitted, ref.ID)
	b.WriteString(content[:head])
	fmt.Fprintf(&b, "\n\n... %d bytes omitted — read_result(id=%q, offset=%d) continues here ...\n\n",
		omitted, ref.ID, head)
	b.WriteString(content[len(content)-tail:])
	return b.String()
}

// truncateFallback is the old behaviour, kept only for when spilling is
// unavailable. It says the bytes are gone rather than implying with "..." that
// something merely ended.
func truncateFallback(content string, threshold int) string {
	if len(content) <= threshold {
		return content
	}
	return content[:threshold] + fmt.Sprintf(
		"\n\n... truncated: %d further bytes were discarded and cannot be retrieved ...",
		len(content)-threshold)
}
