package memory

// compose.go — answering from what is currently known, rather than repeating
// what was once said.
//
// The answer cache and this file look superficially similar: both avoid an LLM
// call by producing an answer locally. They are opposites in the way that
// matters.
//
// The cache REPLAYS. It stores the text of a past answer and hands the same
// bytes back later. That text was true when it was written and nothing about
// storing it keeps it true, so the whole design becomes a fight against
// staleness: time-to-live windows, re-ask detection, admission classes. Every
// one of those is a guess about how fast the world moves, and the guess is
// wrong for some question.
//
// This COMPOSES. It reads the knowledge graph, the episodic record and the
// ranked memory hits at the moment the question is asked, and builds the answer
// out of whatever is there right now. If a symbol moved, the graph moved with
// it; if a decision was superseded, the newer fact outranks the older one. The
// answer cannot go stale between the question and the reply, because there is
// no interval in between — which is why this needs no TTL, no expiry class and
// no re-ask escape hatch. The staleness problem is not mitigated here, it is
// structurally absent.
//
// What it must never do is guess. Everything below is assembled from stored
// facts and cited back to their provenance; where the evidence does not cover
// the question, the composer declines and the request escalates to a model.
// A local answer that is confidently wrong costs more trust than an LLM call
// costs money.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
)

// ComposedAnswer is an answer built from current local knowledge, with the
// evidence that produced it.
type ComposedAnswer struct {
	Text       string
	Confidence core.Confidence
	// SourceNodeIDs are the KG facts that backed the answer, so a rejection
	// can demote exactly those rather than the whole rung.
	SourceNodeIDs []string
}

const (
	// composeMinCoverage is the share of the question's content terms that must
	// appear in the gathered evidence before an answer is offered. Below this
	// the composer is padding around a gap, and a partial answer delivered with
	// full confidence is the failure mode worth avoiding.
	composeMinCoverage = 0.75

	// composeMinFacts is the number of distinct supporting facts required. One
	// matching node is a coincidence as often as it is an answer.
	composeMinFacts = 2

	// composeMaxFacts bounds how much evidence is rendered, so a heavily
	// connected entity produces an answer and not a database dump.
	composeMaxFacts = 8

	// composeMinFactConfidence drops facts that write-back governance has
	// demoted. A fact that has already been rejected once should not quietly
	// come back as part of a composed answer.
	composeMinFactConfidence = 0.4
)

// evidence is one supporting fact gathered for a query.
type evidence struct {
	nodeID     string
	kind       string // symbol | package | fact | decision | fix | episode
	label      string
	detail     string
	provenance string
	confidence float64
	at         time.Time
}

// ComposeAnswer builds an answer to query from the knowledge graph and memory,
// or reports (nil, false) when the local evidence does not cover the question.
//
// It answers only QUESTIONS. A command asks for the world to change, and no
// amount of stored knowledge about the world satisfies it — see isImperative in
// replay.go, which this deliberately shares so the two paths cannot drift into
// disagreeing about what a command is.
func (h *HybridRetriever) ComposeAnswer(kg core.KnowledgeGraphStore, query string) (*ComposedAnswer, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || isImperative(q) || IsSmalltalk(q) {
		return nil, false
	}
	terms := contentTokens(q)
	if len(terms) < 2 {
		return nil, false
	}

	ev := h.gatherEvidence(kg, terms)
	if len(ev) < composeMinFacts {
		return nil, false
	}

	covered, coverage := coverageOf(terms, ev)
	if coverage < composeMinCoverage {
		return nil, false
	}

	// Strongest, most recent evidence first — the reader sees the best support
	// for the claim rather than whatever the graph happened to iterate first.
	sort.SliceStable(ev, func(i, j int) bool {
		if ev[i].confidence != ev[j].confidence {
			return ev[i].confidence > ev[j].confidence
		}
		return ev[i].at.After(ev[j].at)
	})
	if len(ev) > composeMaxFacts {
		ev = ev[:composeMaxFacts]
	}

	text, ids := renderEvidence(query, ev)
	return &ComposedAnswer{
		Text:          text,
		SourceNodeIDs: ids,
		Confidence: core.Confidence{
			Score: scoreFor(coverage, ev),
			Reason: fmt.Sprintf("composed from %d stored fact(s) covering %d/%d query terms, read at question time",
				len(ev), covered, len(terms)),
			Provenance: provenanceOf(ev),
		},
	}, true
}

// gatherEvidence collects facts whose label or detail mentions a query term.
func (h *HybridRetriever) gatherEvidence(kg core.KnowledgeGraphStore, terms []string) []evidence {
	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}

	var out []evidence
	seen := map[string]bool{}

	if kg != nil {
		for _, t := range []core.KGNodeType{
			core.KGNodeSymbol, core.KGNodePackage, core.KGNodeFile,
			core.KGNodeFact, core.KGNodeDecision, core.KGNodeFix,
		} {
			for _, n := range kg.FindByType(t) {
				if n == nil || seen[n.ID] {
					continue
				}
				// An unscored legacy node is neutral, not bad; only an
				// explicitly demoted one is dropped.
				if n.Confidence > 0 && n.Confidence < composeMinFactConfidence {
					continue
				}
				detail := nodeDetail(n)
				if !mentionsAny(want, n.Label) && !mentionsAny(want, detail) {
					continue
				}
				seen[n.ID] = true
				out = append(out, evidence{
					nodeID: n.ID, kind: string(n.Type), label: n.Label, detail: detail,
					provenance: n.Provenance, confidence: confidenceOr(n.Confidence, 0.6),
					at: laterOf(n.LastSeen, n.CreatedAt),
				})
			}
		}
	}

	// Episodic lessons are the record of what actually happened, which is
	// evidence the graph does not carry. Only the lesson is used, never the
	// stored answer text — replaying that is exactly what this file exists to
	// stop doing.
	if h.mem != nil {
		for _, e := range h.mem.EpisodicGet() {
			if e.Outcome != "success" || len(e.LessonsLearned) == 0 {
				continue
			}
			if !mentionsAny(want, e.TaskGoal) {
				continue
			}
			lesson := strings.TrimSpace(strings.Join(e.LessonsLearned, "; "))
			if lesson == "" || seen["ep:"+e.ID] {
				continue
			}
			seen["ep:"+e.ID] = true
			out = append(out, evidence{
				nodeID: e.ID, kind: "episode", label: e.TaskGoal, detail: lesson,
				provenance: "task " + e.ID, confidence: 0.55, at: e.Timestamp,
			})
		}
	}
	return out
}

func nodeDetail(n *core.KGNode) string {
	if n.Properties == nil {
		return ""
	}
	for _, key := range []string{"detail", "summary", "description", "statement", "signature", "receiver"} {
		if v := strings.TrimSpace(n.Properties[key]); v != "" {
			return v
		}
	}
	return ""
}

// coverageOf reports how many query terms the evidence actually mentions.
// This is the guard against confident padding: an answer assembled from facts
// that between them never mention half the question is not an answer to that
// question.
func coverageOf(terms []string, ev []evidence) (int, float64) {
	if len(terms) == 0 {
		return 0, 0
	}
	blob := make([]string, 0, len(ev)*3)
	for _, e := range ev {
		blob = append(blob, e.label, e.detail, e.provenance)
	}
	hay := wordPad(strings.ToLower(strings.Join(blob, " ")))
	covered := 0
	for _, t := range terms {
		if strings.Contains(hay, " "+t+" ") {
			covered++
		}
	}
	return covered, float64(covered) / float64(len(terms))
}

// scoreFor turns coverage and evidence strength into the cascade's comparable
// confidence. Capped below 1.0: a composed answer is well-supported, never
// certain, and the cap keeps a rung threshold able to disable it.
func scoreFor(coverage float64, ev []evidence) float64 {
	var sum float64
	for _, e := range ev {
		sum += e.confidence
	}
	mean := sum / float64(len(ev))
	score := 0.5*coverage + 0.5*mean
	if score > 0.95 {
		score = 0.95
	}
	return score
}

// renderEvidence writes the answer. It is a presentation of stored facts with
// their sources, not generated prose: every line traces to a node the reader
// can go and check, which is what makes an LLM-free answer trustworthy rather
// than merely cheap.
func renderEvidence(query string, ev []evidence) (string, []string) {
	var b strings.Builder
	var ids []string

	b.WriteString("From this project's knowledge graph and history:\n\n")
	for _, e := range ev {
		ids = append(ids, e.nodeID)
		b.WriteString("- **")
		b.WriteString(e.label)
		b.WriteString("** (")
		b.WriteString(e.kind)
		b.WriteString(")")
		if e.detail != "" {
			b.WriteString(" — ")
			b.WriteString(e.detail)
		}
		if e.provenance != "" {
			b.WriteString("  \n  _source: ")
			b.WriteString(e.provenance)
			b.WriteString("_")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nThis was assembled from stored facts as they are right now, not from a " +
		"cached reply — ask again for a model's interpretation if you need one.")
	return b.String(), ids
}

func provenanceOf(ev []evidence) []string {
	var out []string
	for _, e := range ev {
		if e.provenance != "" {
			out = append(out, e.provenance)
		}
	}
	return dedupStrings(out)
}

func mentionsAny(want map[string]bool, text string) bool {
	if text == "" {
		return false
	}
	for _, t := range contentTokens(text) {
		if want[t] {
			return true
		}
	}
	return false
}

func confidenceOr(v, fallback float64) float64 {
	if v <= 0 {
		return fallback
	}
	return v
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
