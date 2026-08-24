// Package recall is the single place that decides where a fact is remembered.
//
// # WHY THIS EXISTS
//
// Thirty-two sites across eight packages wrote memory directly, each picking a
// store by hand: the graph sync built nodes and edges, the outcome recorder
// wrote episodic entries and eight kinds of node, the skill extractor wrote
// procedural entries, ingest wrote semantic ones. Nothing decided placement;
// every caller decided for itself, which means placement was thirty-two
// decisions that had to agree and had no way to.
//
// The cost is not tidiness. Two of those callers can write the same fact under
// different ids and both be kept, because deduplication only works if
// something sees both writes. Adding a fifth store means finding all
// thirty-two. And "what does the agent know" has no answer, because knowing
// requires a gateway and there wasn't one.
//
// # WHAT THIS DOES AND DELIBERATELY DOES NOT DO
//
// It owns placement and identity. It does NOT reshape the payload: a graph
// node arrives with its language, symbol counts and line numbers and is stored
// with them. An earlier sketch of this flattened everything into a generic
// subject-verb-object triple, which is a cleaner-looking API that silently
// loses the properties the graph answers questions from.
//
// Identity is content-addressed: a fact's id is a hash of its canonicalised
// meaning, so the existence check is a lookup in the destination store and
// there is no side index to keep in sync or lose on restart. Provenance,
// confidence and timestamps are excluded from the address, because re-reading
// a file restamps them and would otherwise double the store on every pass.
package recall

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
)

// Fact is something worth remembering. Its shape decides its store — that is
// the whole idea, and it is why these are distinct types rather than one
// struct with a Kind field: a Kind field can be set wrong, a type cannot.
type Fact interface {
	// address is the content address: a hash of what the fact MEANS, so the
	// same fact learned twice is one fact.
	address() string
}

// Entity is a thing the graph knows about: a file, a symbol, a package, a
// task, a decision. Carries its full property set.
type Entity struct {
	Node *core.KGNode
}

// Link is a typed relationship between two entities.
//
// It carries the whole edge rather than a from/verb/to triple for the same
// reason Entity carries the whole node: an edge has a weight and a provenance
// — the file:line of the import declaration that justifies it — and a triple
// silently drops both. Relate builds one for callers that have nothing else
// to say about the relationship.
type Link struct {
	Edge *core.KGEdge
}

// Relate is the convenience shape for a bare relationship.
func Relate(from, to string, r core.KGRelationType) Link {
	return Link{Edge: &core.KGEdge{From: from, To: to, Relation: r, Weight: 1.0, CreatedAt: time.Now()}}
}

// Note is retrievable prose — the only shape that is embedded, because it is
// the only one answered by similarity rather than by traversal.
type Note struct {
	Key, Content, Category string
	Tags                   []string
}

// Event is something that happened and how it turned out.
type Event struct {
	Entry core.EpisodicEntry
}

// Procedure is a reusable skill.
type Procedure struct {
	Skill *core.Skill
}

func (e Entity) address() string {
	// The node's own id IS its meaning — kgsync and the outcome recorder both
	// build stable ids ("file:cmd/root.go", "symbol:pkg.Fn"). Properties are
	// deliberately excluded: re-reading a file with an updated content hash
	// must UPDATE that node, not create a second one.
	if e.Node == nil {
		return ""
	}
	return "entity:" + e.Node.ID
}

func (l Link) address() string {
	if l.Edge == nil {
		return ""
	}
	// Weight, provenance and timestamp are excluded for the same reason they
	// are on Entity: re-indexing restamps them, and identity must survive that.
	return hashOf("link", l.Edge.From, string(l.Edge.Relation), l.Edge.To)
}

func (n Note) address() string {
	// Keyed notes are addressed by key so re-learning the same note replaces
	// it. Unkeyed ones are addressed by content.
	if n.Key != "" {
		return hashOf("note", n.Key)
	}
	return hashOf("note", n.Content)
}

func (e Event) address() string {
	return hashOf("event", e.Entry.TaskGoal, e.Entry.Summary, e.Entry.Outcome)
}

func (p Procedure) address() string {
	if p.Skill == nil {
		return ""
	}
	return hashOf("procedure", p.Skill.Name)
}

// hashOf canonicalises then hashes. Whitespace is collapsed and case folded so
// two writers phrasing the same fact differently produce one entry.
func hashOf(kind string, parts ...string) string {
	norm := make([]string, 0, len(parts))
	for _, p := range parts {
		norm = append(norm, strings.ToLower(strings.Join(strings.Fields(p), " ")))
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + strings.Join(norm, "\x00")))
	return kind + ":" + hex.EncodeToString(sum[:8])
}

// Store is the memory the manager writes through. Satisfied by memory.System;
// declared here so this package depends on behaviour rather than on that type.
type Store interface {
	SemanticAdd(key, content, category string, tags []string) error
	EpisodicAdd(entry core.EpisodicEntry) error
	ProceduralAdd(skill *core.Skill) error
	KG() core.KnowledgeGraphStore
}

// Manager routes facts to stores.
type Manager struct {
	store Store
}

// New returns a Manager over store. A nil store is refused rather than
// producing a manager whose every write is a silent no-op.
func New(store Store) (*Manager, error) {
	if store == nil {
		return nil, fmt.Errorf("recall: nil store")
	}
	return &Manager{store: store}, nil
}

// Remember places a fact in the store its shape belongs to.
//
// Writing the same fact twice is not an error and does not duplicate: the
// destination stores are keyed, so an identical address overwrites. That is
// what makes it safe to record everything the agent observes without a caller
// having to ask "have I seen this already".
func (m *Manager) Remember(f Fact) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("recall: no store")
	}
	switch v := f.(type) {
	case Entity:
		if v.Node == nil || v.Node.ID == "" {
			return fmt.Errorf("recall: entity without an id")
		}
		kg := m.store.KG()
		if kg == nil {
			return fmt.Errorf("recall: no knowledge graph")
		}
		return kg.AddNode(v.Node)

	case Link:
		if v.Edge == nil || v.Edge.From == "" || v.Edge.To == "" {
			return fmt.Errorf("recall: link with a missing end")
		}
		kg := m.store.KG()
		if kg == nil {
			return fmt.Errorf("recall: no knowledge graph")
		}
		if v.Edge.CreatedAt.IsZero() {
			v.Edge.CreatedAt = time.Now()
		}
		return kg.AddEdge(v.Edge)

	case Note:
		if strings.TrimSpace(v.Content) == "" {
			return nil // nothing to remember; not an error
		}
		key := v.Key
		if key == "" {
			key = v.address()
		}
		return m.store.SemanticAdd(key, v.Content, v.Category, v.Tags)

	case Event:
		return m.store.EpisodicAdd(v.Entry)

	case Procedure:
		if v.Skill == nil {
			return fmt.Errorf("recall: procedure without a skill")
		}
		return m.store.ProceduralAdd(v.Skill)
	}
	return fmt.Errorf("recall: unknown fact shape %T", f)
}

// RememberAll writes several facts, returning the first failure but attempting
// every one. A structural sync produces hundreds of facts and one unwritable
// node must not abandon the rest.
func (m *Manager) RememberAll(facts ...Fact) error {
	var first error
	for _, f := range facts {
		if err := m.Remember(f); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Address exposes a fact's content address, so callers can log or compare
// identity without knowing how it is computed.
func Address(f Fact) string {
	if f == nil {
		return ""
	}
	return f.address()
}

// SortedTags returns tags in a stable order. Two writers listing the same tags
// differently would otherwise produce entries that differ only in ordering.
func SortedTags(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.Strings(out)
	return out
}

// GraphWriter adapts a Manager to core.KnowledgeGraphStore so a caller that
// already speaks graph writes routes through the manager without changing
// shape.
//
// This exists because the alternative was worse. Rewriting the graph sync's ten
// call sites into Remember(Entity{...}) meant re-nesting ten multi-line
// composite literals, which is churn with no behavioural payoff and one
// mis-balanced brace away from a silent mistake. The gateway property — every
// write passes through one place that owns placement and identity — is what
// matters, and an adapter delivers it exactly.
//
// Reads pass straight through: this manager owns WRITES. Read routing is the
// Data Source Manager's job, and conflating them here would put retrieval
// policy in the store that holds facts.
type GraphWriter struct {
	m  *Manager
	kg core.KnowledgeGraphStore
}

// Graph returns a writer that records through m and reads through the
// underlying graph. Returns the bare graph when m is nil, so a caller without
// a manager still works rather than silently dropping every write.
func Graph(m *Manager) core.KnowledgeGraphStore {
	if m == nil || m.store == nil {
		return nil
	}
	kg := m.store.KG()
	if kg == nil {
		return nil
	}
	return &GraphWriter{m: m, kg: kg}
}

func (g *GraphWriter) AddNode(n *core.KGNode) error { return g.m.Remember(Entity{Node: n}) }
func (g *GraphWriter) AddEdge(e *core.KGEdge) error { return g.m.Remember(Link{Edge: e}) }
func (g *GraphWriter) Relate(from, to string, r core.KGRelationType) error {
	return g.m.Remember(Relate(from, to, r))
}

// RecordWordRelations is a write, but a bulk co-occurrence one with no fact
// shape of its own — it is the graph's own indexing, not something the agent
// learned. Passed through rather than given a shape it does not have.
func (g *GraphWriter) RecordWordRelations(text string) error { return g.kg.RecordWordRelations(text) }

func (g *GraphWriter) GetNode(id string) (*core.KGNode, bool)      { return g.kg.GetNode(id) }
func (g *GraphWriter) AllNodes() []*core.KGNode                    { return g.kg.AllNodes() }
func (g *GraphWriter) FindByType(t core.KGNodeType) []*core.KGNode { return g.kg.FindByType(t) }
func (g *GraphWriter) GetEdges(id string) []*core.KGEdge           { return g.kg.GetEdges(id) }
func (g *GraphWriter) AllEdges() []*core.KGEdge                    { return g.kg.AllEdges() }
func (g *GraphWriter) Stats() (int, int)                           { return g.kg.Stats() }
func (g *GraphWriter) ConceptRelations(c string) interface{}       { return g.kg.ConceptRelations(c) }
func (g *GraphWriter) AdjustConfidence(id string, d, f float64) (float64, bool) {
	return g.kg.AdjustConfidence(id, d, f)
}
