package memory

// fileobs.go — what the agent has actually seen of each file, and whether it
// has changed since.
//
// WHY THIS EXISTS
//
// The knowledge graph already recorded a file node per source file, stamped
// with the commit it was indexed at, and StaleFiles compared that commit to
// HEAD. That answered "which of my beliefs predate the current revision",
// which sounds like the right question and is not:
//
//   - One commit invalidates EVERY file's belief, including the files that
//     commit did not touch. The honest answer is buried in false positives, so
//     nobody reads it.
//   - Uncommitted edits are invisible. HEAD does not move when a file is
//     saved, so the graph keeps asserting the old contents — including after
//     the agent edited the file ITSELF, which is the single most likely reason
//     for a belief to be wrong mid-task.
//
// A content hash answers the question exactly: this file is different from
// when I looked, that one is not, and it does not care whether the change was
// committed, stashed, or made thirty seconds ago by the agent.
//
// It is fed by what the agent reads and writes rather than by a separate
// indexing pass, so knowledge tracks attention: the files it has actually
// looked at are the files it knows the state of, and every read refreshes that.
//
// Scoped per workspace, because a file index shared between projects describes
// neither.

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
)

// fileHashProperty is the KG file-node property holding the content hash of
// the version the agent last saw. Stored beside the existing "commit" property
// rather than replacing it: a commit still says something useful about
// provenance, it just cannot answer staleness.
const fileHashProperty = "content_hash"

// fileSeenProperty records when that content was last observed.
const fileSeenProperty = "seen_at"

// ContentHash returns the address of a file's contents. Short because it is
// compared, never inverted.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}

// ObserveFile records that the agent has seen path with these contents.
//
// Called on every successful read AND every successful write, because an
// agent that edits a file and does not update what it believes about it is
// wrong about the thing it just changed.
//
// Best-effort: a graph that refuses the write leaves the previous belief in
// place, which is stale rather than absent, and no tool call should fail
// because bookkeeping did.
func (s *System) ObserveFile(workspace, path, content string) {
	kg := s.KG()
	if kg == nil || path == "" {
		return
	}
	rel := relativeTo(workspace, path)
	if rel == "" || strings.HasPrefix(rel, "..") {
		return // outside the workspace: not this project's knowledge
	}

	id := "file:" + rel
	props := map[string]string{
		fileHashProperty: ContentHash(content),
		fileSeenProperty: time.Now().UTC().Format(time.RFC3339),
	}
	// Preserve whatever the indexer already recorded (language, commit,
	// symbol counts) — this observes content, it does not re-describe the file.
	if existing, ok := kg.GetNode(id); ok && existing != nil {
		for k, v := range existing.Properties {
			if k != fileHashProperty && k != fileSeenProperty {
				props[k] = v
			}
		}
	}

	_ = kg.AddNode(&core.KGNode{
		ID:         id,
		Label:      rel,
		Type:       core.KGNodeFile,
		Properties: props,
		Provenance: rel,
		Confidence: 1.0,
		LastSeen:   time.Now(),
	})
}

// FileChanged reports whether path differs from what the agent last saw.
//
// known is false when the agent has never read the file, which is a different
// answer from "unchanged" and must not be confused with it: an unread file is
// not fresh knowledge, it is no knowledge.
func (s *System) FileChanged(workspace, path, currentContent string) (changed, known bool) {
	kg := s.KG()
	if kg == nil {
		return false, false
	}
	rel := relativeTo(workspace, path)
	n, ok := kg.GetNode("file:" + rel)
	if !ok || n == nil {
		return false, false
	}
	prev := n.Properties[fileHashProperty]
	if prev == "" {
		return false, false
	}
	return prev != ContentHash(currentContent), true
}

// relativeTo expresses path relative to workspace, using forward slashes so a
// node id is the same on every platform.
func relativeTo(workspace, path string) string {
	if workspace == "" {
		return filepath.ToSlash(path)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspace, abs)
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
