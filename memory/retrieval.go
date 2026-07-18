package memory

// ============================================================================
// HYBRID RETRIEVER — lightweight semantic-ish recall without embeddings.
//
// Why this exists (RAG/KG assessment):
// The project deliberately has zero non-stdlib dependencies (only readline).
// Full vector-RAG would require an embedding model — either a per-write API
// call (latency + cost + provider coupling) or a heavy local model (bulk).
// That violates the "do not make the project too bulky for no use" rule.
//
// Instead, this retriever upgrades the existing keyword recall (which was
// plain strings.Contains — no ranking, no recall of semantically-close-but-
// not-identical phrasing) to a ranked hybrid scorer:
//
//   score = tokenOverlap(query, entry)        // TF-style overlap (main signal)
//         + recencyBonus(entry)               // prefer recent, but weakly
//         + kgBoost(entry)                    // boost entries whose entities
//                                             // appear in the query (KG graph)
//
// This captures ~80% of RAG's recall-quality benefit (the agent recalls a past
// "build a calculator" task when asked to "create an arithmetic tool") at 0%
// dependency bulk. It is pure Go, stdlib only.
//
// The Knowledge Graph (memory/knowledge_graph.go) is the other half of the
// "hybrid": it contributes the kgBoost signal and remains the structured
// entity-relationship store. So the architecture is: KG (structured) +
// HybridRetriever (ranked free-text recall) = a hybrid of KG + lightweight RAG.
// ============================================================================

import (
	"fmt"
	"github.com/darkcode/internal/strutil"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/darkcode/core"
)

// RecallHit is a ranked recall result.
type RecallHit struct {
	Source    string // "episodic" | "semantic"
	ID        string // episodic id or semantic key
	Goal      string // task goal / semantic key (for display)
	Snippet   string // summary / content (truncated)
	Score     float64
	Timestamp time.Time
}

// HybridRetriever ranks episodic + semantic memory entries by relevance to a
// query. It is safe for concurrent use (it only reads through the System's
// own locked accessors).
type HybridRetriever struct {
	mem core.MemoryStore
	kg  core.KnowledgeGraphStore // optional; may be nil
}

// NewHybridRetriever builds a retriever over the given memory system. The
// knowledge graph is optional; when present it supplies the kgBoost signal.
func NewHybridRetriever(mem core.MemoryStore, kg core.KnowledgeGraphStore) *HybridRetriever {
	return &HybridRetriever{mem: mem, kg: kg}
}

// Recall returns the top-k relevant past entries (episodic + semantic) for the
// query, best-first. Entries with zero overlap are excluded. k is clamped to
// [0,20]; pass 0 to get nothing.
func (h *HybridRetriever) Recall(query string, k int) []RecallHit {
	if k <= 0 || h.mem == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	if k > 20 {
		k = 20
	}
	qTokens := tokenize(query)
	if len(qTokens) == 0 {
		return nil
	}
	
	// Get query embedding if available
	queryVec, _ := h.mem.GetEmbedding(query)
	hasVec := len(queryVec) > 0

	// Computed once per Recall() call and reused for every entry below —
	// previously kgBoost scanned and re-tokenized every KG node's label for
	// EACH entry (O(entries × KG nodes), with the KG capped at 4000 concept
	// nodes). The set of nodes matching the query doesn't depend on which
	// entry we're scoring, so there's no need to recompute it per entry.
	qKGMatches := h.kgQueryMatches(qTokens)

	now := time.Now()
	var hits []RecallHit

	// Episodic memory.
	for _, e := range h.mem.EpisodicGet() {
		// Score with the vector path only when BOTH sides have a vector;
		// entries written before an embedder was configured fall back to
		// keyword overlap and must be gated by the overlap threshold, not the
		// cosine one — previously hasVec alone selected the threshold, so a
		// legacy vectorless entry scoring 0.25 overlap was silently dropped
		// by the 0.3 cosine gate the moment an embedder came online.
		usedVec := hasVec && len(e.Vector) > 0
		var score float64
		if usedVec {
			score = cosineSimilarity(queryVec, e.Vector)
		} else {
			text := e.TaskGoal + " " + e.Summary + " " + strings.Join(e.ToolsUsed, " ")
			score = overlapScore(qTokens, tokenize(text))
		}
		if belowRecallThreshold(score, usedVec) {
			continue
		}

		// Recency: up to +0.15, decaying over ~30 days.
		score += recencyBonus(e.Timestamp, now, 30*24*time.Hour, 0.15)
		// KG boost: if any KG node label overlaps the query, nudge.
		score += kgBoostFromMatches(qKGMatches, e.TaskGoal)
		hits = append(hits, RecallHit{
			Source: "episodic", ID: e.ID, Goal: e.TaskGoal,
			Snippet: strutil.Truncate(e.Summary, 240), Score: score, Timestamp: e.Timestamp,
		})
	}

	// Semantic memory.
	for _, s := range h.mem.SemanticAll() {
		usedVec := hasVec && len(s.Vector) > 0 // see episodic loop comment
		var score float64
		if usedVec {
			score = cosineSimilarity(queryVec, s.Vector)
		} else {
			text := s.Key + " " + s.Content + " " + s.Category + " " + strings.Join(s.Tags, " ")
			score = overlapScore(qTokens, tokenize(text))
		}
		if belowRecallThreshold(score, usedVec) {
			continue
		}

		score += recencyBonus(s.CreatedAt, now, 30*24*time.Hour, 0.15)
		score += kgBoostFromMatches(qKGMatches, s.Key)
		hits = append(hits, RecallHit{
			Source: "semantic", ID: s.Key, Goal: s.Key,
			Snippet: strutil.Truncate(s.Content, 240), Score: score, Timestamp: s.CreatedAt,
		})
	}

	// Rank: score desc, then recency desc.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Timestamp.After(hits[j].Timestamp)
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// ExactRecall returns the most recent successful output for a goal that
// matches query after normalization, enabling the agent to answer repeated
// questions without calling the LLM. Only matches entries with
// Outcome=="success" and a non-empty Output. Returns ("", false) when no
// match. This is the "without LLM" half of the knowledge-reuse architecture:
// recall injection (before LLM) + exact cache (without LLM). Only used in
// General (no-tools) mode — tool-using tasks are never cached because
// filesystem state may have changed.
//
// Matching is on normalizeGoal(query), not the raw string: previously a
// literal `==` comparison meant trivial rephrasing ("fix the bug" vs "fix
// the bug.") missed the cache entirely even though it's clearly the same
// request. Normalization only collapses whitespace/case/trailing punctuation
// — it deliberately does NOT do fuzzy/near-duplicate matching here (that
// risks serving a wrong cached answer for a genuinely different query with
// high apparent confidence). See ConfidentRecall for the bounded,
// strict-threshold near-duplicate extension of this cache, and
// Recall()/kgBoost for the separate, deliberately lenient ranked-context
// path (which only ever informs a prompt, never skips the LLM, so a weak
// match there costs nothing).
//
// maxAge bounds how old a cached answer can be (0 = no limit).
func (h *HybridRetriever) ExactRecall(query string, maxAge time.Duration) (string, bool) {
	if h.mem == nil || strings.TrimSpace(query) == "" {
		return "", false
	}
	normQuery := normalizeGoal(query)
	if normQuery == "" {
		return "", false
	}
	now := time.Now()
	for _, e := range h.mem.EpisodicGet() { // already most-recent-first
		if normalizeGoal(e.TaskGoal) != normQuery {
			continue
		}
		if e.Outcome != "success" || e.Output == "" {
			continue
		}
		if len(e.ToolsUsed) > 0 {
			continue // never cache tool-using tasks (side effects may differ)
		}
		if maxAge > 0 && now.Sub(e.Timestamp) > maxAge {
			continue
		}
		return e.Output, true
	}
	return "", false
}

// confidentRecallJaccardThreshold is the bidirectional token-set similarity
// (intersection / union) required for ConfidentRecall to treat a past task
// as "the same request" for LLM-skip purposes. 0.85 tolerates minor
// rewording/reordering ("fix the login bug" vs "fix the bug in login") but
// rejects anything that merely shares a topic or a few keywords — a query
// that's topically related but NOT this strict a match still gets full LLM
// reasoning, just with the existing Recall()/kgBoost context injection.
const confidentRecallJaccardThreshold = 0.85

// confidentRecallMinTokens guards against short queries (e.g. "fix it",
// "run tests") where a handful of shared tokens could clear the Jaccard
// threshold against an equally short but substantively different past goal
// purely by coincidence — the threshold alone isn't reliable below this
// length.
const confidentRecallMinTokens = 4

// ConfidentRecall extends ExactRecall to also match near-identical (not
// merely normalized-exact) past requests, skipping the LLM call entirely for
// a request that's essentially a repeat of a prior successful no-tool task —
// not just topically similar. It tries ExactRecall first (cheap, and the
// safest possible match), then falls back to a strict bidirectional
// token-Jaccard comparison against every eligible episodic entry.
//
// This is deliberately conservative, on purpose: a false positive here means
// serving a wrong answer with full confidence and zero LLM
// verification — worse than the false negative of one avoidable LLM call.
// That's why the threshold is a strict, fixed 0.85 rather than the KG-boosted
// heuristic scoring Recall() uses for context injection (where a bad match
// only costs a few wasted context tokens, not a wrong final answer) — the
// two paths have very different failure costs and deliberately use different
// (and non-interchangeable) matching strategies as a result.
func (h *HybridRetriever) ConfidentRecall(query string, maxAge time.Duration) (string, bool) {
	if out, ok := h.ExactRecall(query, maxAge); ok {
		return out, ok
	}
	if h.mem == nil || strings.TrimSpace(query) == "" {
		return "", false
	}
	qTokens := tokenize(query)
	if len(qTokens) < confidentRecallMinTokens {
		return "", false
	}
	qset := make(map[string]bool, len(qTokens))
	for _, t := range qTokens {
		qset[t] = true
	}

	now := time.Now()
	for _, e := range h.mem.EpisodicGet() { // most-recent-first
		if e.Outcome != "success" || e.Output == "" || len(e.ToolsUsed) > 0 {
			continue
		}
		if maxAge > 0 && now.Sub(e.Timestamp) > maxAge {
			continue
		}
		if tokenJaccard(qset, tokenize(e.TaskGoal)) >= confidentRecallJaccardThreshold {
			return e.Output, true
		}
	}
	return "", false
}

// belowRecallThreshold gates a candidate entry by the scorer that actually
// ran for it: cosine scores need ≥0.3 to count as semantically related, while
// keyword-overlap scores only need to be positive (ranking sorts the rest).
func belowRecallThreshold(score float64, usedVec bool) bool {
	if usedVec {
		return score <= 0.3
	}
	return score <= 0
}

// GoalSimilarity returns the bidirectional token-Jaccard similarity between
// two goal strings in [0,1], using the same tokenizer the retriever scores
// with. Exported for the cascade's repeat-question detection (a user
// immediately re-asking a locally-answered question is the negative-label
// signal for threshold calibration).
func GoalSimilarity(a, b string) float64 {
	aTokens := tokenize(a)
	if len(aTokens) == 0 {
		return 0
	}
	aset := make(map[string]bool, len(aTokens))
	for _, t := range aTokens {
		aset[t] = true
	}
	return tokenJaccard(aset, tokenize(b))
}

// tokenJaccard computes intersection-over-union between qset and eTokens.
func tokenJaccard(qset map[string]bool, eTokens []string) float64 {
	if len(qset) == 0 || len(eTokens) == 0 {
		return 0
	}
	eset := make(map[string]bool, len(eTokens))
	for _, t := range eTokens {
		eset[t] = true
	}
	inter := 0
	for t := range qset {
		if eset[t] {
			inter++
		}
	}
	union := len(qset) + len(eset) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// normalizeGoal canonicalizes a task goal for exact-cache matching: lowercase,
// collapse internal whitespace runs to a single space, trim leading/trailing
// whitespace, and drop trailing sentence punctuation. This is intentionally
// narrow (no stemming, no stopword removal, no reordering) so it only merges
// requests that are unambiguously the same, never ones that merely share
// topic/keywords — that broader similarity is Recall()'s job, not this one.
func normalizeGoal(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, ".!? ")
	var b strings.Builder
	lastWasSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !lastWasSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			lastWasSpace = true
			continue
		}
		b.WriteRune(r)
		lastWasSpace = false
	}
	return strings.TrimSpace(b.String())
}

// maxRecallBlockLen caps the total size of a FormatRecall block before it's
// injected into a prompt. Per-hit snippets/goals are already truncated, but
// with no overall ceiling a caller passing a large k (or many long-Snippet
// hits) could still blow out a chunk of the context window.
const maxRecallBlockLen = 2000

// FormatRecall renders the hits as a compact markdown block suitable for
// injection into an LLM prompt. Returns "" if there are no hits. The result
// is capped at maxRecallBlockLen; remaining hits are summarized as a count
// rather than dropped silently.
func FormatRecall(hits []RecallHit) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Relevant Past Context (hybrid recall)\n")
	shown := 0
	for _, h := range hits {
		var line strings.Builder
		line.WriteString("- [")
		line.WriteString(h.Source)
		line.WriteString("] ")
		line.WriteString(strutil.Truncate(h.Goal, 100))
		if h.Snippet != "" {
			line.WriteString(" — ")
			line.WriteString(h.Snippet)
		}
		line.WriteString("\n")

		if sb.Len()+line.Len() > maxRecallBlockLen {
			break
		}
		sb.WriteString(line.String())
		shown++
	}
	if shown < len(hits) {
		sb.WriteString(fmt.Sprintf("- (%d more result(s) omitted for length)\n", len(hits)-shown))
	}
	return sb.String()
}

// tokenize splits s into lowercased word tokens, dropping stopwords and
// tokens shorter than 3 chars. This is the unit of overlap scoring.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			t := b.String()
			if !isStopword(t) {
				tokens = append(tokens, t)
			}
		}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// overlapScore computes a TF-style overlap: fraction of query tokens that
// appear in the doc token set, weighted slightly by repeated matches. Range
// [0, ~1.2].
func overlapScore(qTokens, dTokens []string) float64 {
	if len(qTokens) == 0 || len(dTokens) == 0 {
		return 0
	}
	dset := make(map[string]int, len(dTokens))
	for _, t := range dTokens {
		dset[t]++
	}
	var hits float64
	for _, t := range qTokens {
		if n := dset[t]; n > 0 {
			// Diminishing returns for repeated doc tokens.
			hits += 1.0 / float64(n)
		}
	}
	return hits / float64(len(qTokens))
}

// recencyBonus returns a value in [0, max] that decays linearly to 0 over
// `halfLife`. Older-than-halfLife entries get 0.
func recencyBonus(t, now time.Time, halfLife time.Duration, max float64) float64 {
	if t.IsZero() {
		return 0
	}
	age := now.Sub(t)
	if age <= 0 {
		return max
	}
	if age >= halfLife {
		return 0
	}
	return max * (1 - float64(age)/float64(halfLife))
}

// kgQueryMatchCap bounds how many query-matching KG nodes are expanded, and
// kgNeighborCap how many 1-hop neighbors each contributes. With the code
// index in the graph (thousands of symbol nodes) an uncapped expansion could
// turn one generic query token into a huge boost set.
const (
	kgQueryMatchCap = 20
	kgNeighborCap   = 8
)

// kgQueryMatches returns the tokenized labels of every KG node that overlaps
// qTokens, PLUS the labels of each matching node's 1-hop neighbors
// (graph-assisted retrieval, upgrade plan Phase C): a query about "router"
// also boosts entries that mention the files/symbols/concepts the graph
// links to router, which neither keyword overlap nor vectors alone surface.
// Call once per Recall(), not per entry — see the comment at its call site.
// Returns nil if there's no KG attached.
func (h *HybridRetriever) kgQueryMatches(qTokens []string) [][]string {
	if h.kg == nil {
		return nil
	}
	qset := make(map[string]bool, len(qTokens))
	for _, t := range qTokens {
		qset[t] = true
	}
	var matches [][]string
	var matchedIDs []string
	for _, node := range h.kg.AllNodes() {
		nodeTokens := tokenize(node.Label)
		for _, lt := range nodeTokens {
			if qset[lt] {
				matches = append(matches, nodeTokens)
				if len(matchedIDs) < kgQueryMatchCap {
					matchedIDs = append(matchedIDs, node.ID)
				}
				break
			}
		}
	}

	// Neighborhood expansion: include the labels of nodes adjacent to a
	// direct match, so the boost reflects graph structure, not just label
	// overlap. Deduped; a neighbor that already matched isn't re-added.
	seen := make(map[string]bool, len(matchedIDs))
	for _, id := range matchedIDs {
		seen[id] = true
	}
	for _, id := range matchedIDs {
		neighbors := 0
		for _, e := range h.kg.GetEdges(id) {
			other := e.To
			if other == id {
				other = e.From
			}
			if seen[other] {
				continue
			}
			seen[other] = true
			if n, ok := h.kg.GetNode(other); ok {
				if toks := tokenize(n.Label); len(toks) > 0 {
					matches = append(matches, toks)
					neighbors++
				}
			}
			if neighbors >= kgNeighborCap {
				break
			}
		}
	}
	return matches
}

// kgBoostFromMatches scores one entry against the query-matching KG nodes
// already computed by kgQueryMatches: +0.05 per matching node's label that
// also appears in entryText, capped at 0.15.
func kgBoostFromMatches(qKGMatches [][]string, entryText string) float64 {
	if len(qKGMatches) == 0 {
		return 0
	}
	entryTokens := tokenize(entryText)
	eset := make(map[string]bool, len(entryTokens))
	for _, t := range entryTokens {
		eset[t] = true
	}

	var boost float64
	for _, nodeTokens := range qKGMatches {
		eMatch := false
		for _, lt := range nodeTokens {
			if eset[lt] {
				eMatch = true
				break
			}
		}
		if eMatch {
			boost += 0.05
		}
		if boost >= 0.15 {
			break
		}
	}
	return boost
}

// isStopword filters the most common English noise tokens so they don't
// dominate overlap scoring. Kept tiny to avoid bulk.
func isStopword(t string) bool {
	switch t {
	case "the", "and", "for", "with", "that", "this", "from", "have", "your",
		"you", "are", "was", "but", "not", "all", "can", "had", "her",
		"how", "what", "when", "who", "will", "into", "out", "use", "using",
		"create", "make", "want", "need", "like", "get", "set", "put", "run":
		return true
	}
	return false
}

// cosineSimilarity calculates the cosine similarity between two vectors.
// Returns 0 if either vector is empty or length 0.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}
