package memory

// kg_query.go — structural queries over the code knowledge graph, plus the
// two operations that make its confidence scores mean something: propagation
// to neighbours, and versioning against the git commit a fact was read at.

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkcode/infra/core"
)

// Match is one query hit, carrying enough context to cite it.
type Match struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	Type       string  `json:"type"`
	Kind       string  `json:"kind,omitempty"`
	Language   string  `json:"language,omitempty"`
	Provenance string  `json:"provenance,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Relation   string  `json:"relation,omitempty"` // set when reached via an edge
}

func toMatch(n *core.KGNode) Match {
	m := Match{
		ID: n.ID, Label: n.Label, Type: string(n.Type),
		Provenance: n.Provenance, Confidence: n.Confidence,
	}
	if n.Properties != nil {
		m.Kind, m.Language = n.Properties["kind"], n.Properties["language"]
	}
	return m
}

// Search returns nodes whose label contains the (case-insensitive) term,
// optionally restricted to a node type. Results are ordered by confidence so
// exact indexed facts outrank inferred ones.
func (kg *KnowledgeGraph) Search(term string, nodeType core.KGNodeType, limit int) []Match {
	term = strings.ToLower(strings.TrimSpace(term))
	var out []Match
	for _, n := range kg.AllNodes() {
		if nodeType != "" && n.Type != nodeType {
			continue
		}
		if term != "" && !strings.Contains(strings.ToLower(n.Label), term) {
			continue
		}
		out = append(out, toMatch(n))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Neighbors returns the nodes directly connected to id, annotated with the
// relation that reached them and the direction it points.
func (kg *KnowledgeGraph) Neighbors(id string) []Match {
	var out []Match
	for _, e := range kg.GetEdges(id) {
		other, rel := e.To, string(e.Relation)
		if e.To == id {
			other, rel = e.From, string(e.Relation)+" (incoming)"
		}
		if n, ok := kg.GetNode(other); ok {
			m := toMatch(n)
			m.Relation = rel
			out = append(out, m)
		}
	}
	return out
}

// LowConfidence returns the beliefs scoring below threshold, weakest first.
// Surfacing these is what lets the agent say "I am unsure about X" instead of
// asserting a stale fact — a node scores 0 only when it was never scored, so
// those are excluded rather than reported as maximally doubtful.
func (kg *KnowledgeGraph) LowConfidence(threshold float64, limit int) []Match {
	var out []Match
	for _, n := range kg.AllNodes() {
		if n.Confidence > 0 && n.Confidence < threshold {
			out = append(out, toMatch(n))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence < out[j].Confidence })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// PropagateConfidence adjusts a node's confidence and carries a decaying share
// of that adjustment to its neighbours out to maxHops.
//
// The rationale is that evidence is rarely local: if a file turns out to be
// misunderstood, the symbols it defines are suspect too, but less so, and
// their callers less again. Each hop multiplies the delta by decay, so the
// effect dies out instead of washing over the whole graph. Returns the number
// of nodes whose confidence moved.
func (kg *KnowledgeGraph) PropagateConfidence(id string, delta, decay float64, maxHops int) int {
	if maxHops < 0 || delta == 0 {
		return 0
	}
	if decay <= 0 || decay >= 1 {
		decay = 0.5
	}

	changed := 0
	visited := map[string]bool{id: true}
	frontier := []string{id}

	for hop := 0; hop <= maxHops && len(frontier) > 0; hop++ {
		for _, nodeID := range frontier {
			if _, moved := kg.AdjustConfidence(nodeID, delta, 0); moved {
				changed++
			}
		}
		var next []string
		for _, nodeID := range frontier {
			for _, e := range kg.GetEdges(nodeID) {
				other := e.To
				if other == nodeID {
					other = e.From
				}
				if !visited[other] {
					visited[other] = true
					next = append(next, other)
				}
			}
		}
		frontier, delta = next, delta*decay
	}
	return changed
}

// StaleFiles returns the file nodes whose contents differ from what the agent
// last saw — the graph's honest answer to "which of my beliefs are about a
// version of this file that no longer exists?".
//
// It compares CONTENT, not the commit. Comparing the recorded commit to HEAD,
// which is what this used to do, is wrong in both directions: one commit marks
// every file stale including the ones it did not touch, and an uncommitted
// edit marks nothing stale at all — so a file the agent had just edited itself
// still read as current. Content answers exactly, and does not care whether the
// change was committed.
//
// A file the agent has never read is not listed. Never-read is not stale; it
// is unknown, and reporting it here would bury the files that genuinely
// changed under every file in the repository.
func (kg *KnowledgeGraph) StaleFiles(workspace string) []Match {
	var out []Match
	for _, n := range kg.FindByType(core.KGNodeFile) {
		seen := n.Properties[fileHashProperty]
		if seen == "" {
			continue // never observed; see above
		}
		data, err := os.ReadFile(filepath.Join(workspace, n.Label))
		if err != nil {
			m := toMatch(n)
			m.Provenance = "read as " + seen + ", now unreadable (moved or deleted)"
			out = append(out, m)
			continue
		}
		if now := ContentHash(string(data)); now != seen {
			m := toMatch(n)
			m.Provenance = "read as " + seen + ", now " + now
			out = append(out, m)
		}
	}
	return out
}

// GitHead returns the workspace's current commit sha, or "" outside a
// repository. The index sync stamps it onto each file node so the graph knows
// which revision its beliefs were formed at.
func GitHead(workspace string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
