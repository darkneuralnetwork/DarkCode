package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// ============================================================================
// KNOWLEDGE GRAPH — Entity-Relationship Store
//
// In-memory adjacency list with JSON persistence. Provides a graph of
// concepts, files, tools, agents, and tasks connected by typed relationships.
// ============================================================================

// KnowledgeGraph is an in-memory entity-relationship graph.
type KnowledgeGraph struct {
	mu          sync.RWMutex
	nodes       map[string]*core.KGNode
	edges       []*core.KGEdge
	edgeIndex   map[string]int              // O(1) lookup for findEdgeIndexLocked
	adjacent    map[string][]string         // nodeID -> connected node IDs
	edgesByNode map[string][]*core.KGEdge   // nodeID -> incident edges, for O(1) GetEdges
	filePath    string
	writer      *DebouncedWriter
	embedder    core.LLMClient
}

// kgData is the serialized form for JSON persistence.
type kgData struct {
	Nodes []*core.KGNode `json:"nodes"`
	Edges []*core.KGEdge `json:"edges"`
}

// maxConceptNodes caps the number of concept (word) nodes in the graph so
// unbounded word-relation extraction can't grow the KG forever. When
// exceeded, the least-connected concept nodes are pruned.
const maxConceptNodes = 4000

// ConceptRelation is a weighted relation between a concept word and its
// neighbor. Returned by ConceptRelations for the memory tool's kg query.
type ConceptRelation struct {
	Label    string  `json:"label"`
	Weight   float64 `json:"weight"`
	Relation string  `json:"relation"`
}

// NewKnowledgeGraph creates a knowledge graph with JSON persistence.
func NewKnowledgeGraph(dir string) (*KnowledgeGraph, error) {
	path := filepath.Join(dir, "knowledge_graph.json")
	kg := &KnowledgeGraph{
		nodes:       make(map[string]*core.KGNode),
		adjacent:    make(map[string][]string),
		edgesByNode: make(map[string][]*core.KGEdge),
		filePath:    path,
	}
	if err := kg.load(); err != nil {
		return nil, err
	}

	kg.rebuildEdgeIndexLocked()

	// Startup pruning: a persisted graph may already be over
	// maxConceptNodes (e.g. the cap was lowered since the file was last
	// written, or the file was edited/merged externally). Previously
	// pruning only ran inside RecordWordRelations on a write, so an
	// over-cap graph stayed over-cap — and therefore slower to query via
	// the per-node scans in cli/console.go's /know command — until the
	// next write happened to push it further over the cap and trigger a
	// cleanup. pruneConceptsLocked() is a no-op (and cheap: an O(nodes)
	// scan) when already under the cap.
	kg.mu.Lock()
	kg.pruneConceptsLocked()
	kg.mu.Unlock()

	kg.writer = NewDebouncedWriter(path, 2*time.Second, func() ([]byte, error) {
		kg.mu.RLock()
		defer kg.mu.RUnlock()
		stored := kgData{
			Nodes: make([]*core.KGNode, 0, len(kg.nodes)),
			Edges: kg.edges,
		}
		for _, node := range kg.nodes {
			stored.Nodes = append(stored.Nodes, node)
		}
		return json.Marshal(stored) // using non-indent for speed
	})

	return kg, nil
}

// Shutdown flushes pending writes to disk.
func (kg *KnowledgeGraph) Shutdown() {
	if kg.writer != nil {
		kg.writer.Shutdown()
	}
}

func makeEdgeKey(a, b string, rel core.KGRelationType) string {
	if a > b {
		a, b = b, a
	}
	return string(rel) + ":" + a + ":" + b
}

func (kg *KnowledgeGraph) rebuildEdgeIndexLocked() {
	kg.edgeIndex = make(map[string]int, len(kg.edges))
	kg.edgesByNode = make(map[string][]*core.KGEdge, len(kg.nodes))
	for i, e := range kg.edges {
		kg.edgeIndex[makeEdgeKey(e.From, e.To, e.Relation)] = i
		kg.edgesByNode[e.From] = append(kg.edgesByNode[e.From], e)
		if e.To != e.From {
			kg.edgesByNode[e.To] = append(kg.edgesByNode[e.To], e)
		}
	}
}

// RecordWordRelations extracts concept (word) nodes and their co-occurrence
// relations from a body of text and records them in the knowledge graph.
//
// Words are tokenized (lowercased, ≥4 chars, non-stopword — reusing the
// retriever's tokenizer). Words that co-occur within the same sentence/line
// are linked with a `related_to` edge whose weight is the co-occurrence count
// (incremented on repeat). Concept nodes use the `concept` type.
//
// This is the "word relations" layer of the KG: it captures which concepts
// the agent has reasoned about together, so later queries can traverse the
// graph (e.g. "what is related to 'authentication'?"). It is batched: all
// nodes/edges are built in-memory and persisted in a single save so a task
// with 30 word pairs doesn't trigger 60 JSON rewrites.
//
// Path-like tokens never appear (the tokenizer splits on '/' and '.'), so
// concept nodes don't duplicate file nodes.
func (kg *KnowledgeGraph) RecordWordRelations(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	kg.mu.Lock()
	defer kg.mu.Unlock()

	now := time.Now()
	// Track concept nodes created/seen in THIS call so we can cap + prune.
	localConcepts := make(map[string]bool)

	// Process sentence-by-sentence so co-occurrence is meaningful (within a
	// sentence/line, not across the whole text).
	for _, line := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '.' || r == '!' || r == '?'
	}) {
		tokens := tokenize(line)
		// Further filter to concept-worthy tokens (≥4 chars). tokenize already
		// drops stopwords and <3-char tokens.
		var concepts []string
		seen := make(map[string]bool)
		for _, t := range tokens {
			if len(t) < 4 || seen[t] {
				continue
			}
			seen[t] = true
			concepts = append(concepts, t)
		}
		// Link every pair of co-occurring concepts.
		for i := 0; i < len(concepts); i++ {
			for j := i + 1; j < len(concepts); j++ {
				kg.upsertConceptEdgeLocked(concepts[i], concepts[j], now)
				localConcepts[concepts[i]] = true
				localConcepts[concepts[j]] = true
			}
		}
	}

	// Cap concept nodes; prune least-connected if we exceeded the budget.
	if kg.conceptCountLocked() > maxConceptNodes {
		kg.pruneConceptsLocked()
	}

	kg.writer.MarkDirty()
	return nil
}

// upsertConceptEdgeLocked ensures both concept nodes exist and that the
// `related_to` edge between them has its weight incremented. Must be called
// with kg.mu held.
func (kg *KnowledgeGraph) upsertConceptEdgeLocked(a, b string, now time.Time) {
	aID := "concept:" + a
	bID := "concept:" + b
	if aID == bID {
		return
	}
	if _, ok := kg.nodes[aID]; !ok {
		kg.nodes[aID] = &core.KGNode{
			ID: aID, Label: a, Type: core.KGNodeConcept, CreatedAt: now,
		}
	}
	if _, ok := kg.nodes[bID]; !ok {
		kg.nodes[bID] = &core.KGNode{
			ID: bID, Label: b, Type: core.KGNodeConcept, CreatedAt: now,
		}
	}
	// Find an existing edge (either direction) and increment its weight.
	if idx := kg.findEdgeIndexLocked(aID, bID, core.KGRelRelatedTo); idx >= 0 {
		kg.edges[idx].Weight++
		return
	}
	newEdge := &core.KGEdge{
		From: aID, To: bID, Relation: core.KGRelRelatedTo, Weight: 1.0, CreatedAt: now,
	}
	kg.edges = append(kg.edges, newEdge)
	kg.edgeIndex[makeEdgeKey(aID, bID, core.KGRelRelatedTo)] = len(kg.edges) - 1
	kg.adjacent[aID] = append(kg.adjacent[aID], bID)
	kg.adjacent[bID] = append(kg.adjacent[bID], aID)
	kg.edgesByNode[aID] = append(kg.edgesByNode[aID], newEdge)
	kg.edgesByNode[bID] = append(kg.edgesByNode[bID], newEdge)
}

// findEdgeIndexLocked returns the index of an edge between a and b (either
// direction) with the given relation, or -1. Must be called with kg.mu held.
func (kg *KnowledgeGraph) findEdgeIndexLocked(a, b string, rel core.KGRelationType) int {
	if idx, ok := kg.edgeIndex[makeEdgeKey(a, b, rel)]; ok {
		return idx
	}
	return -1
}

// conceptCountLocked counts concept nodes. Must be called with kg.mu held.
func (kg *KnowledgeGraph) conceptCountLocked() int {
	var n int
	for _, node := range kg.nodes {
		if node.Type == core.KGNodeConcept {
			n++
		}
	}
	return n
}

// pruneConceptsLocked removes the least-connected concept nodes (and their
// edges) until the concept count is back under the cap. Keeps the graph
// bounded over long sessions. Must be called with kg.mu held.
func (kg *KnowledgeGraph) pruneConceptsLocked() {
	// Degree per concept node.
	degree := make(map[string]int)
	for _, e := range kg.edges {
		if n, ok := kg.nodes[e.From]; ok && n.Type == core.KGNodeConcept {
			degree[e.From]++
		}
		if n, ok := kg.nodes[e.To]; ok && n.Type == core.KGNodeConcept {
			degree[e.To]++
		}
	}
	// Collect concept node IDs sorted by degree ascending (least-connected
	// first).
	type kv struct {
		id  string
		deg int
	}
	var list []kv
	for id, node := range kg.nodes {
		if node.Type == core.KGNodeConcept {
			list = append(list, kv{id, degree[id]})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].deg < list[j].deg })

	remove := len(list) - maxConceptNodes
	if remove <= 0 {
		return
	}
	toDelete := make(map[string]bool)
	for i := 0; i < remove && i < len(list); i++ {
		toDelete[list[i].id] = true
	}
	for id := range toDelete {
		delete(kg.nodes, id)
		delete(kg.adjacent, id)
	}
	var filtered []*core.KGEdge
	for _, e := range kg.edges {
		if toDelete[e.From] || toDelete[e.To] {
			continue
		}
		filtered = append(filtered, e)
	}
	kg.edges = filtered
	// Clean adjacency lists of deleted references.
	for id, neighbors := range kg.adjacent {
		var cleaned []string
		for _, n := range neighbors {
			if !toDelete[n] {
				cleaned = append(cleaned, n)
			}
		}
		kg.adjacent[id] = cleaned
	}
	kg.rebuildEdgeIndexLocked()
}

// ConceptRelations returns the weighted relations between a concept word and
// its neighbors. The query is matched case-insensitively against concept node
// labels. Used by the memory tool's `kg` action so the agent can explore word
// relations at runtime.
func (kg *KnowledgeGraph) ConceptRelations(query string) interface{} {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	targetID := ""
	kg.mu.RLock()
	defer kg.mu.RUnlock()
	for _, node := range kg.nodes {
		if node.Type == core.KGNodeConcept && strings.ToLower(node.Label) == q {
			targetID = node.ID
			break
		}
	}
	if targetID == "" {
		return nil
	}
	var out []ConceptRelation
	for _, e := range kg.edges {
		var otherID string
		if e.From == targetID {
			otherID = e.To
		} else if e.To == targetID {
			otherID = e.From
		} else {
			continue
		}
		if n, ok := kg.nodes[otherID]; ok {
			w := e.Weight
			if w == 0 {
				w = 1
			}
			out = append(out, ConceptRelation{
				Label: n.Label, Weight: w, Relation: string(e.Relation),
			})
		}
	}
	return out
}

// SetEmbedder injects an LLMClient for generating node embeddings.
func (kg *KnowledgeGraph) SetEmbedder(client core.LLMClient) {
	kg.mu.Lock()
	defer kg.mu.Unlock()
	kg.embedder = client
}

// getEmbedding generates a vector embedding using the registered embedder.
func (kg *KnowledgeGraph) getEmbedding(text string) ([]float32, error) {
	kg.mu.RLock()
	client := kg.embedder
	kg.mu.RUnlock()

	if client == nil {
		return nil, fmt.Errorf("no embedder configured")
	}
	// We use context.Background() since this is internal to memory ops.
	return client.CreateEmbedding(context.Background(), text)
}

// AddNode adds an entity to the knowledge graph.
func (kg *KnowledgeGraph) AddNode(node *core.KGNode) error {
	if vec, err := kg.getEmbedding(node.Label); err == nil {
		node.Vector = vec
	}

	kg.mu.Lock()
	defer kg.mu.Unlock()

	// Re-adding an existing node is an update: keep the original CreatedAt so
	// fact age survives index re-syncs (LastSeen carries freshness instead).
	if prev, ok := kg.nodes[node.ID]; ok && !prev.CreatedAt.IsZero() {
		node.CreatedAt = prev.CreatedAt
	}
	if node.CreatedAt.IsZero() {
		node.CreatedAt = time.Now()
	}
	kg.nodes[node.ID] = node
	kg.writer.MarkDirty()
	return nil
}

// GetNode retrieves a node by ID.
func (kg *KnowledgeGraph) GetNode(id string) (*core.KGNode, bool) {
	kg.mu.RLock()
	defer kg.mu.RUnlock()
	n, ok := kg.nodes[id]
	return n, ok
}

// AdjustConfidence changes a node's stored Confidence by delta, clamped to
// [floor, 1.0]. Returns the new confidence and whether the node was found.
// This is the write-back governance mechanism for episodic-sourced facts
// (fix/decision nodes, local-first upgrade Phase D hardening): when the
// cascade sees a fact's answer get rejected (the user immediately re-asks
// the same question), it demotes that specific fact rather than only
// bumping the whole rung's threshold — see orchestrator/cascade.go's
// detectReAsk. Demotion is permanent (no auto-recovery), matching the
// escalation-only philosophy already used for per-rung thresholds: a
// confidently-wrong local answer costs trust, so the fix should never
// silently regain it.
func (kg *KnowledgeGraph) AdjustConfidence(id string, delta, floor float64) (float64, bool) {
	kg.mu.Lock()
	defer kg.mu.Unlock()
	n, ok := kg.nodes[id]
	if !ok {
		return 0, false
	}
	n.Confidence += delta
	if n.Confidence < floor {
		n.Confidence = floor
	}
	if n.Confidence > 1.0 {
		n.Confidence = 1.0
	}
	kg.writer.MarkDirty()
	return n.Confidence, true
}

// AddEdge creates a relationship between two nodes. Adding an edge that
// already exists (same pair + relation) is an upsert: the existing edge's
// weight is bumped and its provenance refreshed, instead of appending a
// duplicate — this keeps repeated index syncs / repeated tasks idempotent.
func (kg *KnowledgeGraph) AddEdge(edge *core.KGEdge) error {
	kg.mu.Lock()
	defer kg.mu.Unlock()

	if edge.CreatedAt.IsZero() {
		edge.CreatedAt = time.Now()
	}

	// Ensure both nodes exist
	if _, ok := kg.nodes[edge.From]; !ok {
		return fmt.Errorf("source node %s not found", edge.From)
	}
	if _, ok := kg.nodes[edge.To]; !ok {
		return fmt.Errorf("target node %s not found", edge.To)
	}

	// Upsert: an equivalent edge already recorded just gets reinforced.
	if idx := kg.findEdgeIndexLocked(edge.From, edge.To, edge.Relation); idx >= 0 {
		existing := kg.edges[idx]
		if edge.Weight > 0 {
			existing.Weight += edge.Weight
		} else {
			existing.Weight++
		}
		if edge.Provenance != "" {
			existing.Provenance = edge.Provenance
		}
		kg.writer.MarkDirty()
		return nil
	}

	kg.edges = append(kg.edges, edge)
	kg.edgeIndex[makeEdgeKey(edge.From, edge.To, edge.Relation)] = len(kg.edges) - 1
	kg.adjacent[edge.From] = append(kg.adjacent[edge.From], edge.To)
	kg.adjacent[edge.To] = append(kg.adjacent[edge.To], edge.From) // bidirectional
	kg.edgesByNode[edge.From] = append(kg.edgesByNode[edge.From], edge)
	if edge.To != edge.From {
		kg.edgesByNode[edge.To] = append(kg.edgesByNode[edge.To], edge)
	}

	kg.writer.MarkDirty()
	return nil
}

// Relate is a convenience method to add a node and connect it.
func (kg *KnowledgeGraph) Relate(fromID, toID string, relation core.KGRelationType) error {
	return kg.AddEdge(&core.KGEdge{
		From:     fromID,
		To:       toID,
		Relation: relation,
		Weight:   1.0,
	})
}

// FindRelated returns all nodes directly connected to the given node.
func (kg *KnowledgeGraph) FindRelated(nodeID string) []*core.KGNode {
	kg.mu.RLock()
	defer kg.mu.RUnlock()

	related := kg.adjacent[nodeID]
	var result []*core.KGNode
	seen := make(map[string]bool)
	for _, id := range related {
		if seen[id] {
			continue
		}
		seen[id] = true
		if node, ok := kg.nodes[id]; ok {
			result = append(result, node)
		}
	}
	return result
}

// FindByType returns all nodes of a given type.
func (kg *KnowledgeGraph) FindByType(nodeType core.KGNodeType) []*core.KGNode {
	kg.mu.RLock()
	defer kg.mu.RUnlock()

	var result []*core.KGNode
	for _, node := range kg.nodes {
		if node.Type == nodeType {
			result = append(result, node)
		}
	}
	return result
}

// GetEdges returns all edges for a given node (outgoing and incoming).
func (kg *KnowledgeGraph) GetEdges(nodeID string) []*core.KGEdge {
	kg.mu.RLock()
	defer kg.mu.RUnlock()
	return kg.edgesByNode[nodeID]
}

// GetSubgraph returns all nodes within N hops of the given node.
func (kg *KnowledgeGraph) GetSubgraph(nodeID string, maxDepth int) []*core.KGNode {
	kg.mu.RLock()
	defer kg.mu.RUnlock()

	visited := make(map[string]bool)
	var result []*core.KGNode

	queue := []struct {
		id    string
		depth int
	}{{nodeID, 0}}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if visited[item.id] || item.depth > maxDepth {
			continue
		}
		visited[item.id] = true

		if node, ok := kg.nodes[item.id]; ok {
			result = append(result, node)
		}

		for _, neighbor := range kg.adjacent[item.id] {
			if !visited[neighbor] {
				queue = append(queue, struct {
					id    string
					depth int
				}{neighbor, item.depth + 1})
			}
		}
	}
	return result
}

// RemoveNode deletes a node and all its edges.
func (kg *KnowledgeGraph) RemoveNode(nodeID string) error {
	kg.mu.Lock()
	defer kg.mu.Unlock()

	delete(kg.nodes, nodeID)
	delete(kg.adjacent, nodeID)

	// Remove edges involving this node
	var filtered []*core.KGEdge
	for _, edge := range kg.edges {
		if edge.From != nodeID && edge.To != nodeID {
			filtered = append(filtered, edge)
		}
	}
	kg.edges = filtered

	// Clean up adjacency lists
	for id, neighbors := range kg.adjacent {
		var cleaned []string
		for _, n := range neighbors {
			if n != nodeID {
				cleaned = append(cleaned, n)
			}
		}
		kg.adjacent[id] = cleaned
	}

	kg.rebuildEdgeIndexLocked()
	kg.writer.MarkDirty()
	return nil
}

// AllNodes returns all nodes in the graph.
func (kg *KnowledgeGraph) AllNodes() []*core.KGNode {
	kg.mu.RLock()
	defer kg.mu.RUnlock()
	result := make([]*core.KGNode, 0, len(kg.nodes))
	for _, n := range kg.nodes {
		result = append(result, n)
	}
	return result
}

// AllEdges returns all edges in the graph.
func (kg *KnowledgeGraph) AllEdges() []*core.KGEdge {
	kg.mu.RLock()
	defer kg.mu.RUnlock()
	result := make([]*core.KGEdge, len(kg.edges))
	copy(result, kg.edges)
	return result
}

// Stats returns summary statistics.
func (kg *KnowledgeGraph) Stats() (nodeCount, edgeCount int) {
	kg.mu.RLock()
	defer kg.mu.RUnlock()
	return len(kg.nodes), len(kg.edges)
}

// ============================================================================
// PERSISTENCE
// ============================================================================

func (kg *KnowledgeGraph) load() error {
	data, err := os.ReadFile(kg.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var stored kgData
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}

	for _, node := range stored.Nodes {
		kg.nodes[node.ID] = node
	}
	for _, edge := range stored.Edges {
		kg.edges = append(kg.edges, edge)
		kg.adjacent[edge.From] = append(kg.adjacent[edge.From], edge.To)
		kg.adjacent[edge.To] = append(kg.adjacent[edge.To], edge.From)
	}
	return nil
}
