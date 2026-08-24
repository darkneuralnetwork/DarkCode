package memory

// ============================================================================
// HYBRID RETRIEVER — ranked recall over episodic + semantic memory.
//
// An earlier header claimed this worked "without embeddings" and that vector
// RAG had been declined as dependency bulk. That stopped being true once
// vectors were added, and the comment described the opposite of the code.
//
// What actually happens:
//
//   - Recall embeds the query (System.GetEmbedding, 5s bound) and scores each
//     entry by cosine against its stored Vector.
//   - Entries with no vector contribute a tokenOverlap (TF-style) score.
//   - The Knowledge Graph contributes a third signal: entries mentioning a
//     graph neighbour of the query.
//
// So the stack is a genuine KG + RAG hybrid. Ingestion (ingest/) chunks
// sources, embeds each chunk and writes it to semantic memory.
//
// THE THREE SIGNALS ARE FUSED BY RANK, NOT SUMMED.
//
// Cosine, tokenOverlap and the KG score are different units on different
// scales; summing them (as the code once did) let a weak keyword hit outrank a
// strong vector one whenever the store was mixed — which is always, because
// entries written before the embedder was installed never got a vector. fuse()
// instead reads only each signal's ORDER and combines them by reciprocal rank,
// which needs no per-query calibration. Recency stays a small additive
// tie-breaker, not part of the ranked base. See fuse() for the full reasoning.
// ============================================================================

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/darkcode/internal/strutil"

	"github.com/darkcode/infra/core"
)

// RecallHit is a ranked recall result.
type RecallHit struct {
	Source    string // "episodic" | "semantic"
	ID        string // episodic id or semantic key
	Goal      string // task goal / semantic key (for display)
	Snippet   string // summary / content (truncated)
	Score     float64
	Timestamp time.Time
	// Signal names every ranking list this hit appeared in — "vector",
	// "keyword", "kg", or a "+"-joined combination. Provenance is what lets
	// telemetry answer "is the vector signal earning its cost", so the
	// retrieval work can be falsified rather than asserted.
	Signal string
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

	qKGMatches := h.kgQueryMatches(qTokens)
	now := time.Now()
	_ = hasVec // vectors are scored per-candidate below, not gated up front

	// Score every candidate on BOTH signals, then decide the ranking once every
	// entry is in. The old code picked one scorer or the other per entry and
	// pushed the results into a single list sorted by raw score, silently
	// comparing a cosine against a token fraction — see fuse().
	cands := make([]candidate, 0, 64)

	for _, e := range h.mem.EpisodicGet() {
		text := e.TaskGoal + " " + e.Summary + " " + strings.Join(e.ToolsUsed, " ")
		cands = append(cands, candidate{
			hit: RecallHit{
				Source: "episodic", ID: e.ID, Goal: e.TaskGoal,
				Snippet: strutil.Truncate(e.Summary, 240), Timestamp: e.Timestamp,
			},
			vec:     cosineOrZero(queryVec, e.Vector),
			hasVec:  len(queryVec) > 0 && len(e.Vector) > 0,
			keyword: overlapScore(qTokens, tokenize(text)),
			bonus:   recencyBonus(e.Timestamp, now, 30*24*time.Hour, 0.15),
			kgScore: kgBoostFromMatches(qKGMatches, e.TaskGoal),
		})
	}

	for _, s := range h.mem.SemanticAll() {
		text := s.Key + " " + s.Content + " " + s.Category + " " + strings.Join(s.Tags, " ")
		cands = append(cands, candidate{
			hit: RecallHit{
				Source: "semantic", ID: s.Key, Goal: s.Key,
				Snippet: strutil.Truncate(s.Content, 240), Timestamp: s.CreatedAt,
			},
			vec:     cosineOrZero(queryVec, s.Vector),
			hasVec:  len(queryVec) > 0 && len(s.Vector) > 0,
			keyword: overlapScore(qTokens, tokenize(text)),
			bonus:   recencyBonus(s.CreatedAt, now, 30*24*time.Hour, 0.15),
			kgScore: kgBoostFromMatches(qKGMatches, s.Key),
		})
	}

	hits := fuse(cands, k)
	h.noteUse(hits)
	return hits
}

// useRecorder is told which entries a recall returned, so the forgetting curve
// in decay.go is fed by retrieval itself rather than by every caller
// remembering to report. Satisfied by *System.
//
// It is an optional interface rather than a method on core.MemoryStore because
// only one implementation can do anything with it, and widening the store
// interface would make every test double in the repository implement a
// bookkeeping call it does not care about.
type useRecorder interface{ NoteUse(ids []string) }

// noteUse credits the entries a recall surfaced. Retrieval is the only evidence
// available that an entry was ever worth keeping, so this is what separates
// "old" from "unused" when consolidation later has to choose.
func (h *HybridRetriever) noteUse(hits []RecallHit) {
	rec, ok := h.mem.(useRecorder)
	if !ok || len(hits) == 0 {
		return
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.Source == "episodic" && hit.ID != "" {
			ids = append(ids, hit.ID)
		}
	}
	rec.NoteUse(ids)
}

// candidate carries every signal for one entry, so ranking can be decided after
// every entry has been scored rather than per-entry.
type candidate struct {
	hit     RecallHit
	vec     float64 // cosine, 0 when either side has no vector
	hasVec  bool    // whether vec is meaningful
	keyword float64 // token overlap, always available
	bonus   float64 // recency only; small, additive, breaks ties
	kgScore float64 // knowledge-graph neighbourhood match, ranked as a signal
}

// cosineOrZero is cosineSimilarity with the "one side has no vector" case
// folded in, so callers do not have to guard it.
func cosineOrZero(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return cosineSimilarity(a, b)
}

// rrfK damps the contribution of low ranks. 60 is the value from the original
// reciprocal-rank-fusion work and is not sensitive: it mainly decides how much
// a first place beats a tenth.
const rrfK = 60.0

// vectorFloor is the cosine below which a vector hit is not evidence of
// anything. Unchanged from the old per-entry threshold.
const vectorFloor = 0.3

// fuse combines the vector, keyword and knowledge-graph rankings by reciprocal
// rank.
//
// The bug this replaces: each entry was scored by EITHER cosine OR token
// overlap, and both went into one list sorted by raw score. Those are different
// units. Cosine's floor for "related" is 0.3 and good matches sit at 0.6–0.85;
// overlapScore is the fraction of query tokens matched, with a threshold of
// merely > 0. A weak keyword hit at 0.5 therefore outranked a strong vector hit
// at 0.45. It fired whenever the store held a mix of vectored and unvectored
// entries — which was always, because entries written before the embedder
// finished validating never got one.
//
// RRF is used rather than normalising the two scores because it needs no
// calibration: it reads only each list's ORDER. Min-max normalising two
// distributions per query is unstable when one list is short — a single vector
// hit normalises to 1.0 regardless of how weak it is. Rank is immune to that.
//
// Recency stays additive on top as a tie-breaker, where it is small and
// interpretable, instead of being mixed into an incomparable base.
func fuse(cands []candidate, k int) []RecallHit {
	if len(cands) == 0 {
		return nil
	}

	byVec := make([]int, 0, len(cands))
	byKeyword := make([]int, 0, len(cands))
	byKG := make([]int, 0, len(cands))
	for i, c := range cands {
		if c.hasVec && c.vec > vectorFloor {
			byVec = append(byVec, i)
		}
		if c.keyword > 0 {
			byKeyword = append(byKeyword, i)
		}
		if c.kgScore > 0 {
			byKG = append(byKG, i)
		}
	}
	// Each list breaks ties on the entry ID, not on the order the candidates
	// arrived in.
	//
	// That order is not stable: SemanticAll ranges over a map, so two identical
	// queries against an unchanged store produced different candidate orders, a
	// stable sort preserved them for tied entries, and RRF then assigned those
	// entries DIFFERENT ranks — so the fused score itself changed between calls.
	// Recall was returning a different ranking every time for the same question,
	// which makes the answer cache non-reproducible and any measurement of
	// retrieval quality meaningless. Ranking must be a function of the store's
	// contents alone.
	sort.SliceStable(byVec, func(a, b int) bool {
		if cands[byVec[a]].vec != cands[byVec[b]].vec {
			return cands[byVec[a]].vec > cands[byVec[b]].vec
		}
		return cands[byVec[a]].hit.ID < cands[byVec[b]].hit.ID
	})
	sort.SliceStable(byKeyword, func(a, b int) bool {
		if cands[byKeyword[a]].keyword != cands[byKeyword[b]].keyword {
			return cands[byKeyword[a]].keyword > cands[byKeyword[b]].keyword
		}
		return cands[byKeyword[a]].hit.ID < cands[byKeyword[b]].hit.ID
	})
	sort.SliceStable(byKG, func(a, b int) bool {
		if cands[byKG[a]].kgScore != cands[byKG[b]].kgScore {
			return cands[byKG[a]].kgScore > cands[byKG[b]].kgScore
		}
		return cands[byKG[a]].hit.ID < cands[byKG[b]].hit.ID
	})

	type fused struct {
		idx    int
		score  float64
		signal string
	}
	scores := make(map[int]*fused, len(cands))
	add := func(idx, rank int, signal string) {
		f, ok := scores[idx]
		if !ok {
			f = &fused{idx: idx}
			scores[idx] = f
		}
		f.score += 1.0 / (rrfK + float64(rank))
		if f.signal == "" {
			f.signal = signal
		} else if !strings.Contains(f.signal, signal) {
			f.signal += "+" + signal
		}
	}
	for rank, idx := range byVec {
		add(idx, rank+1, "vector")
	}
	for rank, idx := range byKeyword {
		add(idx, rank+1, "keyword")
	}
	// The knowledge graph is a THIRD retrieval signal, ranked alongside the
	// other two rather than added as a nudge afterwards. "This entry mentions
	// something the query's graph neighbourhood contains" is evidence about
	// relevance, and evidence belongs in the ranking, not in the tiebreak.
	for rank, idx := range byKG {
		add(idx, rank+1, "kg")
	}

	type ranked struct {
		hit       RecallHit
		magnitude float64 // total raw signal strength, for tie-breaking
		bonus     float64 // recency, for breaking a true tie
	}
	out := make([]ranked, 0, len(scores))
	for _, f := range scores {
		c := cands[f.idx]
		hit := c.hit
		hit.Score = f.score
		hit.Signal = f.signal
		out = append(out, ranked{hit: hit, magnitude: c.vec + c.keyword + c.kgScore, bonus: c.bonus})
	}

	// Three keys, in this order, and the order is the whole point.
	//
	// RRF reads only each list's ORDER, which is what makes it immune to the
	// incomparable-units problem. The cost is that it also discards MAGNITUDE:
	// two entries that swap ranks across two lists score identically, however
	// far apart their actual scores were. So magnitude breaks the tie, and only
	// then recency — a preference, not evidence, so it belongs last. A final
	// total key on ID keeps the map-ordered output reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].hit.Score != out[j].hit.Score {
			return out[i].hit.Score > out[j].hit.Score
		}
		if out[i].magnitude != out[j].magnitude {
			return out[i].magnitude > out[j].magnitude
		}
		if out[i].bonus != out[j].bonus {
			return out[i].bonus > out[j].bonus
		}
		if !out[i].hit.Timestamp.Equal(out[j].hit.Timestamp) {
			return out[i].hit.Timestamp.After(out[j].hit.Timestamp)
		}
		return out[i].hit.ID < out[j].hit.ID
	})
	if len(out) > k {
		out = out[:k]
	}
	hits := make([]RecallHit, 0, len(out))
	for _, r := range out {
		hits = append(hits, r.hit)
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
		// Admission is decided when the entry is written (replay.go), not
		// here: an identical goal string tells us the request repeats, not
		// that the stored answer is still true. Replayable also subsumes the
		// old blanket "never cache tool-using tasks" rule — a mutating tool
		// makes the entry class never, while a read-only lookup becomes
		// volatile and ages out instead of being excluded outright.
		if !Replayable(e, now) {
			continue
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
		if !Replayable(e, now) {
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
	sb.WriteString("Each fact below carries a citation tag. When you rely on one, cite it as [F1], [F2], … " +
		"so the claim can be traced back. State plainly when something is not supported by these facts.\n")
	shown := 0
	for _, h := range hits {
		var line strings.Builder
		fmt.Fprintf(&line, "- [F%d] [%s] ", shown+1, h.Source)
		line.WriteString(strutil.Truncate(h.Goal, 100))
		if h.Snippet != "" {
			line.WriteString(" — ")
			line.WriteString(h.Snippet)
		}
		// The id is the structural provenance: it points back at the episodic
		// entry or semantic key the fact came from, so a citation resolves to
		// something real rather than to a number the model invented.
		if h.ID != "" {
			line.WriteString(" (source: " + h.ID + ")")
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

// citationTag matches the [F1] references the recall block asks for.
var citationTag = regexp.MustCompile(`\[F(\d+)\]`)

// CitedFacts returns the 1-based fact numbers an answer cited.
func CitedFacts(answer string) []int {
	var out []int
	seen := map[int]bool{}
	for _, m := range citationTag.FindAllStringSubmatch(answer, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// UncitedClaim reports whether an answer asserts something concrete about the
// codebase without citing any of the facts it was given.
//
// This is a nudge, not a verdict: it catches the case where recall supplied
// evidence and the answer made specific structural claims anyway without
// reference to it — the shape of a confident guess. An answer that cites
// nothing because it needed nothing is not flagged, because it makes no such
// claim.
func UncitedClaim(answer string, factsInjected int) bool {
	if factsInjected == 0 || len(CitedFacts(answer)) > 0 {
		return false
	}
	return structuralClaim.MatchString(answer)
}

// structuralClaim matches assertions about where code lives or what it does —
// the claims that ought to rest on an indexed fact.
var structuralClaim = regexp.MustCompile(`(?i)\b(is defined in|is implemented in|lives in|is located in|` +
	`is handled by|calls into|depends on|is called from|according to the code)\b`)

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
	// Widen before multiplying. float64(a[i]*b[i]) does the multiply in
	// float32, which tops out near 3.4e38 — a component around 1e19 squares to
	// +Inf and the whole score comes back NaN, which then sorts
	// unpredictably against every other candidate rather than merely ranking
	// low. No embedder emits components that large, so this is a correctness
	// floor rather than a live bug; it is pinned because the narrow multiply
	// looks harmless and would be easy to write again.
	var dotProduct, normA, normB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dotProduct += x * y
		normA += x * x
		normB += y * y
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}
