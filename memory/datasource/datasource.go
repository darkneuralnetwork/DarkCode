// Package datasource is the single gateway for reads.
//
// # WHY THIS EXISTS
//
// `recall` owns writes: a caller states what it learned and the manager decides
// placement. Nothing owned the other direction. The orchestrator reached into
// `memory` for every kind of read — ranked recall, the answer cache, graph
// question answering, smalltalk, claim checking, blast radius — thirteen
// distinct symbols across seven files. That is why `orchestrator` imported a
// concrete implementation package at all, and why "what do we know about this?"
// had no single answer to point at.
//
// This is the read half of that pair. It composes what already exists —
// HybridRetriever for ranking (reciprocal-rank fusion over vector, keyword and
// graph signals) and the knowledge graph for structured facts — rather than
// reimplementing either. The point is one door, not a new retrieval engine.
//
// # WHAT IT OWNS
//
// Source selection (which store answers a question), retrieval strategy, and
// the downcast to the concrete graph. Ranking and deduplication already live in
// the retriever and are reached through here.
//
// # WHAT IT DELIBERATELY DOES NOT DO
//
// It does not cache. Measured first: a Recall over a 500-entry store is ~4 ms of
// local CPU with no tokens and no network, and the only network cost — the query
// embedding — is already memoised in the store. A cache here would add
// invalidation risk to buy back milliseconds. See docs/ARCHITECTURE_ANALYSIS.md
// §6.1.1 for the numbers and the threshold at which that changes.
package datasource

import (
	"strings"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/memory/memory"
)

// Types re-exported as aliases so callers name this gateway rather than the
// store behind it. Aliases, not definitions: the identity is unchanged, so a
// value crosses the boundary without conversion and the retriever keeps
// returning exactly what it always did.
type (
	// Hit is one ranked recall result.
	Hit = memory.RecallHit
	// GraphAnswer is an answer derived from typed, provenance-carrying facts.
	GraphAnswer = memory.GraphAnswer
	// RecallAnswer is a replayable answer from a prior successful run.
	RecallAnswer = memory.RecallAnswer
	// ComposedAnswer is an answer assembled from several facts.
	ComposedAnswer = memory.ComposedAnswer
	// Support is the graph's verdict on a set of candidate claims.
	Support = memory.Support
	// Impact is the blast radius of a change.
	Impact = memory.Impact
)

// OriginImported marks a skill that was written down rather than learned here.
const OriginImported = memory.OriginImported

// graphReasoner is the slice of the concrete knowledge graph that answers
// questions about itself rather than merely storing nodes. These three are not
// on core.KnowledgeGraphStore because they are reasoning, not storage — and
// three separate call sites in the orchestrator each downcast to the concrete
// type to reach one of them. The downcast happens once, here.
type graphReasoner interface {
	AdjudicateCandidates(candidates []string) (int, []Support)
	BlastRadius(files []string, maxDepth int) Impact
	PropagateConfidence(id string, delta, decay float64, maxHops int) int
}

// Manager is the one way to ask what is known.
type Manager struct {
	mem       core.MemoryStore
	retriever *memory.HybridRetriever
}

// New builds the gateway over a memory store. The retriever is constructed
// here, so the store and the ranking over it cannot get out of step.
func New(mem core.MemoryStore) *Manager {
	if mem == nil {
		return &Manager{}
	}
	return &Manager{mem: mem, retriever: memory.NewHybridRetriever(mem, mem.KG())}
}

// Query asks what is known about a goal. K bounds the ranked hits.
type Query struct {
	Goal string
	K    int
	// SinceEpoch drops conversational entries older than the current session,
	// so a new chat does not resurface a previous one. Durable facts are
	// session-independent and always kept.
	SinceEpoch time.Time
}

// Context is what the gateway found.
type Context struct {
	Hits []Hit
}

// Retrieve is the one way anything asks "what do we know about this".
//
// The epoch filter runs against a wider set than the caller asked for and the
// result is trimmed afterwards, so dropping stale conversation cannot starve
// out durable facts that ranked just below it.
func (m *Manager) Retrieve(q Query) Context {
	if m.retriever == nil || q.K <= 0 {
		return Context{}
	}
	hits := m.retriever.Recall(q.Goal, max(q.K*3, 10))
	if !q.SinceEpoch.IsZero() {
		kept := hits[:0]
		for _, h := range hits {
			// Episodic entries and the "task:"-keyed semantic facts written for
			// every Q&A are prior conversation, not durable knowledge.
			conversational := h.Source == "episodic" || strings.HasPrefix(h.ID, "task:")
			if conversational && h.Timestamp.Before(q.SinceEpoch) {
				continue
			}
			kept = append(kept, h)
		}
		hits = kept
	}
	if len(hits) > q.K {
		hits = hits[:q.K]
	}
	return Context{Hits: hits}
}

// Recall returns the top-k ranked entries for a query, unfiltered.
func (m *Manager) Recall(goal string, k int) []Hit {
	if m.retriever == nil {
		return nil
	}
	return m.retriever.Recall(goal, k)
}

// ConfidentRecall returns a prior answer only on an exact or strict
// near-duplicate match. This is the answer cache.
func (m *Manager) ConfidentRecall(goal string, maxAge time.Duration) (string, bool) {
	if m.retriever == nil {
		return "", false
	}
	return m.retriever.ConfidentRecall(goal, maxAge)
}

// BestRecallAnswer returns the strongest replayable answer for a query.
func (m *Manager) BestRecallAnswer(goal string, toolMaxAge time.Duration) (*RecallAnswer, bool) {
	if m.retriever == nil {
		return nil, false
	}
	return m.retriever.BestRecallAnswer(goal, toolMaxAge)
}

// ComposeAnswer assembles an answer from several stored facts.
func (m *Manager) ComposeAnswer(goal string) (*ComposedAnswer, bool) {
	if m.retriever == nil || m.mem == nil {
		return nil, false
	}
	return m.retriever.ComposeAnswer(m.mem.KG(), goal)
}

// AnswerFromGraph answers from typed facts that carry provenance.
func (m *Manager) AnswerFromGraph(goal string) (*GraphAnswer, bool) {
	if m.mem == nil {
		return nil, false
	}
	return memory.AnswerFromGraph(m.mem.KG(), goal)
}

// SmalltalkReply answers a greeting without retrieval or a model call.
func (m *Manager) SmalltalkReply(msg string) (string, bool) {
	return memory.SmalltalkReply(msg)
}

// FormatRecall renders ranked hits for prompt injection.
func (m *Manager) FormatRecall(hits []Hit) string { return memory.FormatRecall(hits) }

// UncitedClaim reports whether an answer asserts something the injected facts
// do not support. Package-level, like GoalSimilarity: it inspects text and
// needs no store.
func UncitedClaim(answer string, factsInjected int) bool {
	return memory.UncitedClaim(answer, factsInjected)
}

// Skills returns the stored procedures, best-effort.
func (m *Manager) Skills() []*core.Skill {
	if m.mem == nil {
		return nil
	}
	return m.mem.ProceduralAll()
}

// GoalSimilarity is the bidirectional token similarity between two goals, using
// the same tokenizer the retriever ranks with. Package-level because it is a
// pure comparison and needs no store.
func GoalSimilarity(a, b string) float64 { return memory.GoalSimilarity(a, b) }

// reasoner returns the graph as something that can reason about itself, or
// false when the store is not the concrete graph (a test double, or absent).
func (m *Manager) reasoner() (graphReasoner, bool) {
	if m.mem == nil {
		return nil, false
	}
	kg, ok := m.mem.KG().(*memory.KnowledgeGraph)
	if !ok || kg == nil {
		return nil, false
	}
	return kg, true
}

// Adjudicate checks candidate claims against the graph and reports which one
// the evidence supports. best < 0 means the graph could not separate them.
func (m *Manager) Adjudicate(candidates []string) (best int, supports []Support, ok bool) {
	r, has := m.reasoner()
	if !has {
		return -1, nil, false
	}
	b, s := r.AdjudicateCandidates(candidates)
	return b, s, true
}

// BlastRadius reports which files transitively depend on the ones given.
func (m *Manager) BlastRadius(files []string, maxDepth int) (Impact, bool) {
	r, has := m.reasoner()
	if !has {
		return Impact{}, false
	}
	return r.BlastRadius(files, maxDepth), true
}

// PropagateConfidence lowers confidence in beliefs derived from a node and
// returns how many were softened — used after a rollback, when the files a
// belief was formed from no longer say what they said.
func (m *Manager) PropagateConfidence(id string, delta, decay float64, maxHops int) int {
	r, has := m.reasoner()
	if !has {
		return 0
	}
	return r.PropagateConfidence(id, delta, decay, maxHops)
}
