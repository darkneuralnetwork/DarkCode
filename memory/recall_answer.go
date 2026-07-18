package memory

// recall_answer.go — confident episodic recall, the cascade's rung-3 answerer
// (router.RungRecall). Serves ONE specific past successful answer without an
// LLM when the current question is confidently the same question.
//
// Position in the reuse spectrum:
//   - ExactRecall/ConfidentRecall (rung 1) replay only near-verbatim repeats
//     of NO-TOOL answers — goal-to-goal matching at ≥0.85 Jaccard.
//   - This rung is the graded extension: it also matches the answer TEXT, so
//     "who is pm of india" finds the episode whose output says "The current
//     Prime Minister of India is …" even though the goals barely overlap; and
//     it may serve answers produced via read-only informational tools
//     (readOnlyAnswerTools), which rung 1 categorically excludes.
//   - Ranked hybrid recall (Recall) stays a context injector for the LLM
//     paths; anything below this rung's bar informs the prompt, never
//     answers ("graph-first ≠ graph-only").
//
// Confidence policy: when both sides carry embedding vectors, cosine
// similarity is the signal (the principled path once an embedder is
// validated). The vectorless fallback is deterministic keyword coverage:
// EVERY content token of the query must appear in the episode's text, where a
// short token also counts as covered when consecutive content tokens spell it
// as an acronym ("pm" ← "prime minister") — no synonym dictionary, nothing
// invented. Volatile-world safety: tool-derived answers expire (caller's
// toolMaxAge); pure no-tool explanations don't age the same way and don't.
// Mutating-tool episodes are never eligible — replaying a claim that an
// action ran is exactly what the entry-rung classifier exists to prevent.

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/darkcode/core"
)

// RecallAnswer is a rung-3 candidate: one past answer plus the graded
// confidence signal the cascade gates on.
type RecallAnswer struct {
	Output    string    // the past answer text (episodic Output)
	Goal      string    // the past task's goal, for citation
	ID        string    // episodic entry id (Confidence.Provenance)
	Score     float64   // graded confidence in [0,1]
	Reason    string    // human-readable signal description (telemetry)
	Timestamp time.Time // when the answer was produced (staleness display)
	ToolsUsed []string  // how it was produced (citation)
}

// readOnlyAnswerTools are the informational tools whose outputs are safe to
// replay as answers: they observe the world, they don't change it. File and
// terminal tools are deliberately absent — their outputs depend on local
// state that may have changed since (the same reason ExactRecall skips all
// tool-using episodes).
var readOnlyAnswerTools = map[string]bool{
	"web_search": true,
	"web_fetch":  true,
	"memory":     true,
}

const (
	// recallAnswerMinTokens is the minimum content tokens a query needs —
	// a one-token query ("india?") matches far too much to answer from.
	recallAnswerMinTokens = 2
	// recallAnswerMaxOutputLen: only compact outputs are served. A long
	// output is a task artifact, not an answer to a question.
	recallAnswerMaxOutputLen = 2000
	// recallAnswerKeywordScore is the flat confidence of a full-coverage
	// keyword match: deliberately below the cache rung's 0.9 (fuzzier
	// evidence) but above the 0.75 default answer threshold, so the rung
	// starts enabled and two self-calibration threshold raises
	// (orchestrator/cascade.go §9 loop) suffice to disable it.
	recallAnswerKeywordScore = 0.8
	// maxAcronymLen bounds acronym expansion ("pm"…"https"-sized).
	maxAcronymLen = 5
)

// BestRecallAnswer returns the highest-confidence past answer for query, or
// (nil, false) when no eligible episode matches. toolMaxAge bounds how old a
// tool-derived answer may be (0 = no limit); no-tool answers never expire.
// The caller gates the returned Score against its answer threshold.
func (h *HybridRetriever) BestRecallAnswer(query string, toolMaxAge time.Duration) (*RecallAnswer, bool) {
	if h.mem == nil || strings.TrimSpace(query) == "" {
		return nil, false
	}
	qTokens := contentTokens(query)
	if len(qTokens) < recallAnswerMinTokens {
		return nil, false
	}
	queryVec, _ := h.mem.GetEmbedding(query)
	now := time.Now()

	var best *RecallAnswer
	for _, e := range h.mem.EpisodicGet() { // most-recent-first
		if e.Outcome != "success" || e.Output == "" || len(e.Output) > recallAnswerMaxOutputLen {
			continue
		}
		if !answerToolsEligible(e.ToolsUsed) {
			continue
		}
		if len(e.ToolsUsed) > 0 && toolMaxAge > 0 && now.Sub(e.Timestamp) > toolMaxAge {
			continue
		}

		score, reason := 0.0, ""
		if len(queryVec) > 0 && len(e.Vector) > 0 {
			if cos := cosineSimilarity(queryVec, e.Vector); cos > score {
				score, reason = cos, "embedding similarity to a past successful answer"
			}
		}
		// The keyword signal can still beat a weak cosine (Recall()'s
		// vector-preference is for ranking context, where a weak match
		// costs nothing; here the stronger of the two signals decides).
		if score < recallAnswerKeywordScore && answerTextCovers(qTokens, e) {
			score = recallAnswerKeywordScore
			reason = "every query term (or its spelled-out acronym) appears in a past successful answer"
		}
		if reason == "" {
			continue
		}
		// Strict > keeps the most recent candidate on ties (EpisodicGet is
		// most-recent-first).
		if best == nil || score > best.Score {
			best = &RecallAnswer{
				Output:    e.Output,
				Goal:      e.TaskGoal,
				ID:        e.ID,
				Score:     score,
				Reason:    reason,
				Timestamp: e.Timestamp,
				ToolsUsed: e.ToolsUsed,
			}
		}
	}
	return best, best != nil
}

// answerToolsEligible reports whether every tool the episode used is in the
// read-only informational set (an empty list is trivially eligible).
func answerToolsEligible(tools []string) bool {
	for _, t := range tools {
		if !readOnlyAnswerTools[t] {
			return false
		}
	}
	return true
}

// answerTextCovers reports whether EVERY query content token appears in the
// episode's combined goal+summary+output text, counting an acronym expansion
// as an appearance. Full coverage or nothing: partial topical overlap is
// Recall()'s context-injection territory, not answer territory.
func answerTextCovers(qTokens []string, e core.EpisodicEntry) bool {
	dTokens := contentTokens(e.TaskGoal + " " + e.Summary + " " + e.Output)
	if len(dTokens) == 0 {
		return false
	}
	dset := make(map[string]bool, len(dTokens))
	for _, t := range dTokens {
		dset[t] = true
	}
	var acronyms map[string]bool // built lazily — most tokens resolve via dset
	for _, q := range qTokens {
		if dset[q] {
			continue
		}
		if utf8.RuneCountInString(q) <= maxAcronymLen {
			if acronyms == nil {
				acronyms = acronymIndex(dTokens)
			}
			if acronyms[q] {
				continue
			}
		}
		return false
	}
	return true
}

// acronymIndex returns every initialism (2..maxAcronymLen letters) spelled by
// consecutive content tokens of the document, e.g. "prime minister" → "pm",
// "united states of america" → "usa" (stopwords are already dropped by
// contentTokens, matching how acronyms conventionally skip them).
func acronymIndex(dTokens []string) map[string]bool {
	idx := make(map[string]bool)
	for i := range dTokens {
		var b strings.Builder
		for j := i; j < len(dTokens) && j-i < maxAcronymLen; j++ {
			r, _ := utf8.DecodeRuneInString(dTokens[j])
			b.WriteRune(r)
			if j > i {
				idx[b.String()] = true
			}
		}
	}
	return idx
}

// contentTokens is tokenize's sibling for answer matching: it keeps 2-char
// tokens (tokenize's 3-char floor silently drops "pm", "ui", "k8"…) and, in
// exchange, must drop the short function words that floor was implicitly
// filtering.
func contentTokens(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() >= 2 {
			t := b.String()
			if !isStopword(t) && !isShortFunctionWord(t) {
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

// isShortFunctionWord lists the 2-char English function words contentTokens
// must drop explicitly (tokenize never sees them — its 3-char floor drops all
// 2-char tokens, content or not). "go" is deliberately kept: in a coding
// assistant it is a content word.
func isShortFunctionWord(t string) bool {
	switch t {
	case "am", "an", "as", "at", "be", "by", "do", "he", "if", "in", "is",
		"it", "me", "my", "no", "of", "on", "or", "so", "to", "up", "us",
		"we":
		return true
	}
	return false
}
