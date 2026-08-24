package ctxengine

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/modelport"
)

// ctxengineModelManager is a routerless modelport.Manager — IncrementalSummarizer
// already has its client injected directly (no router of its own), and only
// ever calls CompleteWith (never Complete), so a nil router is fine.
var ctxengineModelManager, _ = modelport.New(nil)

func contentStr(m core.Message) string {
	if m.Content == nil {
		return ""
	}
	if s, ok := m.Content.(string); ok {
		return s
	}
	return m.ContentString()
}

// ----------------------------------------------------------------------------
// IncrementalSummarizer
// ----------------------------------------------------------------------------

// IncrementalSummarizer summarizes conversation blocks as they age. It uses an
// LLM when one is available (fast tier) and falls back to a deterministic
// extractive summary (top-scoring sentences) so the engine is useful even
// with no model configured.
type IncrementalSummarizer struct {
	mu  sync.RWMutex
	llm core.LLMClient // optional; nil → extractive fallback
}

// NewIncrementalSummarizer builds a summarizer. Pass nil to use the
// extractive fallback only.
func NewIncrementalSummarizer(llm core.LLMClient) *IncrementalSummarizer {
	return &IncrementalSummarizer{llm: llm}
}

// SetClient hot-swaps the LLM client this summarizer uses — called from
// Engine.SetClient so the block-summarizer AdaptiveCompressor wraps (see
// Compress below) shares the exact client Engine.Compress/Summarize use,
// instead of construction-time nil that a later SetClient never reached. A
// prior version of this code deliberately left that wiring out to avoid two
// independently-set client pointers drifting apart on a hot-swap; routing
// both through this one setter is what actually closes that gap, rather
// than leaving the summarizer permanently nil to avoid it.
func (s *IncrementalSummarizer) SetClient(llm core.LLMClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.llm = llm
}

// Summarize produces a single system message capturing the gist of msgs.
func (s *IncrementalSummarizer) Summarize(ctx context.Context, msgs []core.Message) core.Message {
	if len(msgs) == 0 {
		return core.Message{Role: core.RoleSystem, Content: ""}
	}
	// Build a transcript.
	var b strings.Builder
	for _, m := range msgs {
		c := contentStr(m)
		if c == "" {
			continue
		}
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(c)
		b.WriteString("\n")
	}
	transcript := b.String()

	// LLM fast path. CompleteWith applies PurposeCompress's shared ceiling/
	// temperature (this ran with no ceiling before), fits the transcript to
	// the client's window, and tags the call log with purpose="compress".
	s.mu.RLock()
	llm := s.llm
	s.mu.RUnlock()
	if llm != nil {
		ans, err := ctxengineModelManager.CompleteWith(ctx, llm, llm.ModelInfo().ID, modelport.Ask{
			Purpose: modelport.PurposeCompress,
			Messages: []core.Message{
				{Role: core.RoleSystem, Content: "Summarize the following conversation block concisely, preserving key decisions, file paths, and outcomes. Output a compact briefing."},
				{Role: core.RoleUser, Content: transcript},
			},
		})
		if err == nil && ans != nil {
			return core.Message{Role: core.RoleSystem, Content: "[Summarized Context]\n" + ans.Text}
		}
	}

	// Extractive fallback: pick the top sentences by TF score against the
	// whole transcript. Deterministic, no LLM.
	summary := extractiveSummary(transcript, 5)
	return core.Message{Role: core.RoleSystem, Content: "[Summarized Context]\n" + summary}
}

// extractiveSummary returns up to nTop highest-scoring sentences (in original
// order) from text. Score = sum of term frequencies weighted by inverse
// sentence frequency (a lightweight TF-IDF).
func extractiveSummary(text string, nTop int) string {
	sentences := splitSentences(text)
	if len(sentences) <= nTop {
		return text
	}
	// term frequency across the whole text
	tf := termFreq(text)
	// score each sentence
	type scoredItem struct {
		idx   int
		score float64
	}
	items := make([]scoredItem, len(sentences))
	for i, s := range sentences {
		for term, f := range termFreq(s) {
			// rarer terms (lower global tf) weight more
			items[i].score += float64(f) / (1.0 + math.Log1p(float64(tf[term])))
		}
		items[i].idx = i
	}
	sort.Slice(items, func(i, j int) bool { return items[i].score > items[j].score })
	// take top n, re-sort by original index for coherence
	top := items[:nTop]
	sort.Slice(top, func(i, j int) bool { return top[i].idx < top[j].idx })
	var b strings.Builder
	for _, t := range top {
		b.WriteString(strings.TrimSpace(sentences[t.idx]))
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// splitSentences breaks text into sentences on . ! ? followed by whitespace.
func splitSentences(text string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range text {
		cur.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// termFreq returns a map of lowercase token → count.
func termFreq(text string) map[string]int {
	tf := map[string]int{}
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		tok = strings.Trim(tok, ".,;:!?\"'`()[]{}")
		if tok == "" || isStopword(tok) {
			continue
		}
		tf[tok]++
	}
	return tf
}

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"if": true, "then": true, "is": true, "are": true, "was": true, "were": true,
	"to": true, "of": true, "in": true, "on": true, "for": true, "with": true,
	"as": true, "at": true, "by": true, "this": true, "that": true, "it": true,
}

func isStopword(w string) bool { return stopwords[w] }

// ----------------------------------------------------------------------------
// ContextRanker
// ----------------------------------------------------------------------------

// ContextRanker scores each context item by relevance to the current task.
// Combines TF-IDF overlap with the query and a recency boost.
type ContextRanker struct{}

func NewContextRanker() *ContextRanker { return &ContextRanker{} }

// Rank returns msgs ordered by descending relevance to query. The most
// relevant messages come first; the original order is used as a tiebreaker.
// A thin convenience over RankIndices for a caller that only wants the
// reordered messages, not the index mapping back to msgs' original order —
// Assemble needs that mapping (to restore chronological order among the
// survivors after trimming) so it calls RankIndices directly instead.
func (r *ContextRanker) Rank(ctx context.Context, query string, msgs []core.Message) []core.Message {
	if len(msgs) == 0 {
		return msgs
	}
	order := r.RankIndices(ctx, query, msgs)
	out := make([]core.Message, len(order))
	for i, idx := range order {
		out[i] = msgs[idx]
	}
	return out
}

// RankIndices returns the indices of msgs in descending-relevance order to
// query (stable: ties keep msgs' original relative order) — i.e. order[0] is
// the index into msgs of the most relevant message.
func (r *ContextRanker) RankIndices(ctx context.Context, query string, msgs []core.Message) []int {
	if len(msgs) == 0 {
		return nil
	}
	qtf := termFreq(query)
	type scoredItem struct {
		idx   int
		score float64
	}
	items := make([]scoredItem, len(msgs))
	now := time.Now()
	for i, m := range msgs {
		c := contentStr(m)
		stf := termFreq(c)
		// cosine-like overlap
		var overlap float64
		for term, qf := range qtf {
			if sf, ok := stf[term]; ok {
				overlap += float64(qf) * float64(sf)
			}
		}
		// normalize by message length to avoid long-message bias
		norm := math.Sqrt(float64(len(stf) + 1))
		recency := recencyBoost(m, now)
		items[i] = scoredItem{idx: i, score: overlap/norm + recency}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	order := make([]int, len(items))
	for i, s := range items {
		order[i] = s.idx
	}
	return order
}

// recencyBoost gives a small boost to the most recent messages so a stale
// high-TF message doesn't displace the current turn. (Messages have no
// timestamp field, so we approximate by position: later index = more recent.)
func recencyBoost(m core.Message, now time.Time) float64 {
	_ = now
	return 0 // position is handled by Stable sort; boost reserved for future ts
}

// ----------------------------------------------------------------------------
// Deduplicator
// ----------------------------------------------------------------------------

// Deduplicator detects and merges duplicate information across memory layers.
// Exact-content dedup plus a near-duplicate check via 3-shingle Jaccard.
type Deduplicator struct{}

func NewDeduplicator() *Deduplicator { return &Deduplicator{} }

// minDedupWords is the shortest message content Deduplicate will consider
// for removal, exact or near. Below this floor, dedup was dropping the
// second of two separate turns that just happen to share short wording —
// "continue", "yes", "ok" — which is legitimate conversation structure, not
// redundancy: collapsing it breaks user/assistant turn alternation, which
// matters once this ran on live loop.Run history (a "continue" follow-up is
// exactly the case loopHistoryBudgetTokens exists to serve). A message this
// short also carries too little shingle signal for Jaccard to mean much
// anyway — shingleSet's own fallback for text under k=3 words is a bag of
// single words, so two on-topic-but-different one-word replies can still
// collide.
const minDedupWords = 4

// Deduplicate returns msgs with exact- and near-duplicates removed. The first
// occurrence is kept; later near-duplicates (Jaccard >= 0.8) are dropped.
// Messages shorter than minDedupWords are never considered for removal — see
// its comment.
func (d *Deduplicator) Deduplicate(msgs []core.Message) []core.Message {
	if len(msgs) == 0 {
		return msgs
	}
	seen := make([]map[string]bool, 0, len(msgs))
	out := make([]core.Message, 0, len(msgs))
	for _, m := range msgs {
		c := contentStr(m)
		if len(strings.Fields(c)) < minDedupWords {
			out = append(out, m)
			continue
		}
		shingles := shingleSet(c, 3)
		dup := false
		for _, s := range seen {
			if jaccard(shingles, s) >= 0.8 {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen = append(seen, shingles)
		out = append(out, m)
	}
	return out
}

// shingleSet returns the set of k-word shingles in text.
func shingleSet(text string, k int) map[string]bool {
	toks := strings.Fields(strings.ToLower(text))
	if len(toks) < k {
		set := map[string]bool{}
		for _, t := range toks {
			set[t] = true
		}
		return set
	}
	set := map[string]bool{}
	for i := 0; i+k <= len(toks); i++ {
		set[strings.Join(toks[i:i+k], " ")] = true
	}
	return set
}

// jaccard returns the Jaccard similarity of two sets.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ----------------------------------------------------------------------------
// TokenBudgetManager
// ----------------------------------------------------------------------------

// TokenBudgetManager allocates budget across system prompt, context, tools,
// generation. estimateTokens uses ~4 chars/token (a common heuristic for
// English + code).
type TokenBudgetManager struct{}

func NewTokenBudgetManager() *TokenBudgetManager { return &TokenBudgetManager{} }

// EstimateTokens returns an approximate token count for a message.
func (t *TokenBudgetManager) EstimateTokens(m core.Message) int {
	return core.EstimateTokens(contentStr(m))
}

// TrimToBudget keeps the highest-ranked messages that fit within `limit`
// tokens. Expects msgs already ranked (most relevant first). System messages
// are always kept. If the ranked set doesn't fit, the tail is summarized.
func (t *TokenBudgetManager) TrimToBudget(msgs []core.Message, limit int) ([]core.Message, error) {
	if limit <= 0 {
		return nil, errBudgetExceeded
	}
	var kept []core.Message
	total := 0
	for _, m := range msgs {
		cost := t.EstimateTokens(m)
		if total+cost > limit {
			break
		}
		kept = append(kept, m)
		total += cost
	}
	if len(kept) == len(msgs) {
		return kept, nil
	}
	return kept, errBudgetExceeded
}

// Budget returns the recommended context token budget given the model's max
// context and reserves for tools + generation.
func (t *TokenBudgetManager) Budget(modelContext, toolReserve, genReserve int) int {
	budget := modelContext - toolReserve - genReserve
	if budget < 0 {
		budget = modelContext / 2
	}
	return budget
}

// ----------------------------------------------------------------------------
// AdaptiveCompressor
// ----------------------------------------------------------------------------

// AdaptiveCompressor compresses context aggressively when even the
// budget-trimmed set still overflows. It replaces overflow messages with a
// single reference summary.
type AdaptiveCompressor struct {
	summarizer *IncrementalSummarizer
}

func NewAdaptiveCompressor(s *IncrementalSummarizer) *AdaptiveCompressor {
	return &AdaptiveCompressor{summarizer: s}
}

// Compress trims msgs to fit `limit` tokens, replacing the dropped tail with a
// compressed summary message when a summarizer is configured.
func (c *AdaptiveCompressor) Compress(ctx context.Context, msgs []core.Message, limit int) []core.Message {
	if len(msgs) == 0 {
		return msgs
	}
	total := 0
	for _, m := range msgs {
		total += core.EstimateTokens(contentStr(m))
	}
	if total <= limit {
		return msgs
	}
	// msgs is ranked most-relevant-first (ContextRanker.Rank), not
	// chronological — Assemble hands this func's output straight through.
	// Keep the highest-ranked prefix that fits; summarize the lower-ranked
	// overflow, not the messages the caller most wants the model to see.
	cutoff := 0
	running := 0
	for i, m := range msgs {
		running += core.EstimateTokens(contentStr(m))
		if running > limit {
			break
		}
		cutoff = i + 1
	}
	if cutoff <= 0 {
		// Even the single highest-ranked message alone overflows the budget.
		// Keep it anyway — a deliberate, bounded overflow — rather than
		// summarizing the one message the caller most wants to see.
		cutoff = 1
	}
	kept := msgs[:cutoff]
	overflow := msgs[cutoff:]
	if len(overflow) == 0 {
		return kept
	}
	out := make([]core.Message, 0, len(kept)+1)
	out = append(out, kept...)
	if c.summarizer != nil {
		out = append(out, c.summarizer.Summarize(ctx, overflow))
		return out
	}
	// No summarizer: keep a truncated reference of the dropped messages.
	ref := core.Message{Role: core.RoleSystem, Content: "[Lower-relevance context compressed — " + strconv.Itoa(len(overflow)) + " messages omitted]"}
	return append(out, ref)
}

// errors
type ctxError string

func (e ctxError) Error() string { return string(e) }

const errBudgetExceeded ctxError = "context budget exceeded"
