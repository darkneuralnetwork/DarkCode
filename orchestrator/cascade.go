package orchestrator

// cascade.go — the cognition cascade controller (local-first upgrade §2/§7
// Phase A). Before any LLM work, Execute runs the query down a cost-ascending
// ladder of local answerers, each returning a first-class core.Confidence:
//
//	rung 0 — deterministic tools (go/ast + ripgrep, exact, ~free)
//	rung 1 — answer cache (ConfidentRecall: exact/near-duplicate repeats)
//	rung 2 — knowledge-graph query (typed code/activity facts, cited)
//	rung 3 — confident episodic recall (graded reuse of ONE past answer)
//
// A rung answers only when its confidence clears cascadeAnswerThreshold;
// otherwise the query escalates. Rung 3 answers only from a full-coverage or
// high-cosine match to one specific past successful answer
// (memory/recall_answer.go); ranked hybrid recall below that bar deliberately
// stays a context injector for the LLM rungs (4 local / 5 cloud) rather than
// a direct answerer — see the plan's "graph-first ≠ graph-only".
//
// The entry-point classifier (router.TaskClassifier.EntryRung) picks which
// rung a query STARTS at, so synthesis/action requests skip straight to the
// LLM path and never risk a confidently-wrong cache hit, while structural
// questions get the deterministic tools first.
//
// Every attempt — hit or escalation — is recorded in the kernel's cascade
// log: the per-query "which rung answered and why" telemetry that is both
// the cost-savings proof and the dataset for calibrating the thresholds.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/memory"
	"github.com/darkcode/router"
)

// cascadeDefaultThreshold is the starting minimum confidence a retrieval rung
// needs to answer without escalating. Thresholds are PER RUNG at runtime and
// self-raise from real usage: an immediate re-ask of a locally-answered
// question is treated as a negative label (the answer didn't satisfy), and a
// rung whose retry ratio exceeds cascadeMaxRetryRatio gets its threshold
// bumped — adjustments only ever move toward MORE escalation, because a wrong
// local answer costs trust while an extra LLM call only costs money (§9).
const cascadeDefaultThreshold = 0.75

// recallAnswerToolMaxAge bounds how old a tool-derived (web_search/web_fetch)
// answer may be for rung 3 to replay it: web-sourced facts go stale in a way
// pure no-tool explanations don't. A week keeps genuinely-static lookups
// serveable while the re-ask loop (detectReAsk) stays the escape hatch for
// anything that changed sooner.
const recallAnswerToolMaxAge = 7 * 24 * time.Hour

const (
	// cascadeRetryWindow is how soon a similar follow-up counts as a re-ask
	// of the previous locally-answered question.
	cascadeRetryWindow = 2 * time.Minute
	// cascadeRetrySimilarity is the GoalSimilarity needed to call a follow-up
	// a re-ask rather than a new question.
	cascadeRetrySimilarity = 0.6
	// cascadeMinSamples is how many answers a rung needs before its retry
	// ratio is trusted for threshold adjustment.
	cascadeMinSamples = 5
	// cascadeMaxRetryRatio is the retried/answered ratio above which a rung's
	// threshold is raised.
	cascadeMaxRetryRatio = 0.3
	// cascadeThresholdStep is the per-adjustment threshold increase. A rung
	// whose threshold climbs past every confidence its answerer can produce
	// (max 1.0) is effectively disabled for the session — the intended
	// failsafe for a rung that keeps serving unsatisfying answers.
	cascadeThresholdStep = 0.05
	cascadeThresholdMax  = 1.05
)

// factDemotionDelta/Floor govern fact-level write-back governance
// (local-first upgrade Phase D hardening): when a re-ask rejects an answer
// that came from specific KG facts (fix/decision nodes — see
// memory.GraphAnswer.SourceNodeIDs), those exact facts are demoted, in
// addition to the existing rung-wide threshold bump above. This is more
// precise than the rung-wide mechanism (which would also penalize unrelated
// good facts sharing the same rung) and, like it, is permanent: a demoted
// fact never auto-recovers, matching the "confidently-wrong local answer
// costs trust" principle (§9). The floor sits below any realistic answer
// threshold so a repeatedly-wrong fact simply stops being served, while
// staying in the graph for inspection rather than being deleted.
const (
	factDemotionDelta = -0.15
	factDemotionFloor = 0.3
)

// maxCascadeLog bounds the in-memory rung log.
const maxCascadeLog = 200

// CascadeEntry records one cascade decision for telemetry: which rung a
// query entered at, which rung answered (or that it escalated to the LLM),
// with the confidence signal and latency.
type CascadeEntry struct {
	Time       time.Time       `json:"time"`
	Query      string          `json:"query"`
	EntryRung  int             `json:"entry_rung"`
	Rung       int             `json:"rung"`      // rung that answered; RungLLM when escalated
	RungName   string          `json:"rung_name"` // deterministic | cache | graph | llm
	Answered   bool            `json:"answered"`  // true = answered locally without any LLM call
	Confidence core.Confidence `json:"confidence"`
	LatencyMs  int64           `json:"latency_ms"`
	// Retried marks a locally-answered entry whose question the user re-asked
	// within cascadeRetryWindow — the negative label for calibration.
	Retried bool `json:"retried,omitempty"`
	// RetryOfRungName is set on the forced-escalation entry produced by a
	// re-ask, naming the rung whose answer was rejected. This puts the
	// negative label in the persisted dataset next to the escalation event.
	RetryOfRungName string `json:"retry_of_rung_name,omitempty"`
	// AnsweredNodeIDs are the specific KG fact node IDs that produced a
	// rung-2 answer (memory.GraphAnswer.SourceNodeIDs), so a later re-ask can
	// demote exactly those facts instead of only the whole rung's threshold
	// (write-back governance, local-first upgrade Phase D hardening). Empty
	// for rungs 0/1 and for AST-verified rung-2 answers, which aren't
	// individually demotable.
	AnsweredNodeIDs []string `json:"answered_node_ids,omitempty"`
}

// CascadeRungStats aggregates one rung's lifetime counters plus its current
// (possibly auto-raised) answer threshold.
type CascadeRungStats struct {
	Rung      int     `json:"rung"`
	Name      string  `json:"name"`
	Answered  int     `json:"answered"`
	Retried   int     `json:"retried"`
	Threshold float64 `json:"threshold"`
}

// CascadeLog returns a copy of the recent cascade decisions (newest last).
func (k *Kernel) CascadeLog() []CascadeEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]CascadeEntry, len(k.cascadeLog))
	copy(out, k.cascadeLog)
	return out
}

// SetCascadeLogPath enables JSONL persistence of cascade decisions to path
// (one entry per line, appended). The persisted log survives restarts — it
// is the calibration dataset §9 asks for. "" disables persistence.
func (k *Kernel) SetCascadeLogPath(path string) {
	k.mu.Lock()
	k.cascadeLogPath = path
	k.mu.Unlock()
}

// CascadeStats returns per-rung lifetime counters and the current (possibly
// auto-raised) thresholds — the at-a-glance calibration view.
func (k *Kernel) CascadeStats() []CascadeRungStats {
	names := map[int]string{
		router.RungDeterministic: "deterministic",
		router.RungCache:         "cache",
		router.RungGraph:         "graph",
		router.RungRecall:        "recall",
		router.RungLLM:           "llm",
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []CascadeRungStats
	for _, rung := range []int{router.RungDeterministic, router.RungCache, router.RungGraph, router.RungRecall, router.RungLLM} {
		out = append(out, CascadeRungStats{
			Rung:      rung,
			Name:      names[rung],
			Answered:  k.cascadeRungAnswered[rung],
			Retried:   k.cascadeRungRetried[rung],
			Threshold: k.cascadeThresholds[rung],
		})
	}
	return out
}

// rungThreshold returns the current answer threshold for a rung.
func (k *Kernel) rungThreshold(rung int) float64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cascadeThresholds[rung]
}

func (k *Kernel) recordCascade(e CascadeEntry) {
	k.mu.Lock()
	k.cascadeLog = append(k.cascadeLog, e)
	if len(k.cascadeLog) > maxCascadeLog {
		k.cascadeLog = k.cascadeLog[len(k.cascadeLog)-maxCascadeLog:]
	}
	if e.Answered || e.Rung == router.RungLLM {
		k.cascadeRungAnswered[e.Rung]++
	}
	path := k.cascadeLogPath
	k.mu.Unlock()

	if path != "" {
		persistCascadeEntry(path, e)
	}
}

// persistCascadeEntry appends one JSONL line, best-effort: telemetry must
// never fail a request.
func persistCascadeEntry(path string, e CascadeEntry) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// detectReAsk checks whether goal is an immediate re-ask of the most recent
// LOCALLY-answered question (within cascadeRetryWindow, GoalSimilarity ≥
// cascadeRetrySimilarity). A re-ask means the local answer didn't satisfy:
// the original entry is marked Retried (the calibration negative label), the
// rung's counters are updated, its threshold possibly raised, and the caller
// must force-escalate so the user gets a fresh LLM answer instead of the
// same cached/graph answer again. Returns the rejected rung's name, or "".
func (k *Kernel) detectReAsk(goal string, now time.Time) string {
	rungName := ""
	raisedRung := -1
	var demoteNodeIDs []string

	k.mu.Lock()
	for i := len(k.cascadeLog) - 1; i >= 0; i-- {
		e := &k.cascadeLog[i]
		if !e.Answered {
			continue
		}
		if now.Sub(e.Time) > cascadeRetryWindow || e.Retried ||
			memory.GoalSimilarity(goal, e.Query) < cascadeRetrySimilarity {
			break // most recent local answer isn't a match — not a re-ask
		}
		e.Retried = true
		k.cascadeRungRetried[e.Rung]++
		if k.maybeRaiseThresholdLocked(e.Rung) {
			raisedRung = e.Rung
		}
		rungName = e.RungName
		if len(e.AnsweredNodeIDs) > 0 {
			demoteNodeIDs = append(demoteNodeIDs, e.AnsweredNodeIDs...)
		}
		break
	}
	k.mu.Unlock()

	// Log outside the lock (k.log locks k.mu itself).
	if raisedRung >= 0 {
		k.log("cascade", "Rung "+itoaCascade(raisedRung)+" retry ratio too high — raised its answer threshold (needs higher confidence to answer locally now)")
	}

	// Fact-level demotion (write-back governance, local-first upgrade Phase D
	// hardening): a rejected answer sourced from specific KG facts demotes
	// exactly those facts, in addition to the rung-wide threshold bump above.
	// More precise — it doesn't penalize unrelated good facts sharing rung 2 —
	// and, like the rung threshold, permanent: a demoted fact never
	// auto-recovers.
	if len(demoteNodeIDs) > 0 && k.memory != nil {
		if kg := k.memory.KG(); kg != nil {
			for _, id := range demoteNodeIDs {
				if newConf, ok := kg.AdjustConfidence(id, factDemotionDelta, factDemotionFloor); ok {
					k.log("cascade", fmt.Sprintf("Demoted fact %s to confidence %.2f after rejection", id, newConf))
				}
			}
		}
	}

	return rungName
}

// maybeRaiseThresholdLocked raises a rung's answer threshold when enough
// samples exist and its retry ratio is too high, reporting whether it did.
// Escalation-only by design: thresholds never decrease automatically (§9 — a
// confidently-wrong local answer is worse than paying for the LLM). Must be
// called with k.mu held; must not log (k.log re-locks k.mu).
func (k *Kernel) maybeRaiseThresholdLocked(rung int) bool {
	answered := k.cascadeRungAnswered[rung]
	if answered < cascadeMinSamples {
		return false
	}
	ratio := float64(k.cascadeRungRetried[rung]) / float64(answered)
	if ratio <= cascadeMaxRetryRatio {
		return false
	}
	if k.cascadeThresholds[rung] >= cascadeThresholdMax {
		return false
	}
	k.cascadeThresholds[rung] += cascadeThresholdStep
	return true
}

// runCascade tries to answer goal from the local retrieval rungs. On a hit it
// returns (answer, true); on a miss it logs the escalation and returns
// ("", false) so Execute proceeds down the existing LLM paths. It never calls
// an LLM and never uses mutating tools.
func (k *Kernel) runCascade(ctx context.Context, goal string) (string, bool) {
	start := time.Now()
	entryRung := router.RungLLM
	if k.classifier != nil {
		entryRung = k.classifier.EntryRung(goal)
	}

	retryOf := ""
	finish := func(rung int, name string, conf core.Confidence, answer string, nodeIDs []string) (string, bool) {
		answered := answer != ""
		k.recordCascade(CascadeEntry{
			Time:            start,
			Query:           strutil.Truncate(goal, 120),
			EntryRung:       entryRung,
			Rung:            rung,
			RungName:        name,
			Answered:        answered,
			Confidence:      conf,
			LatencyMs:       time.Since(start).Milliseconds(),
			RetryOfRungName: retryOf,
			AnsweredNodeIDs: nodeIDs,
		})
		if answered {
			k.log("cascade", "Rung "+itoaCascade(rung)+" ("+name+") answered without LLM: "+conf.Reason)
		}
		return answer, answered
	}

	// Re-ask detection (calibration feedback loop): if the user immediately
	// re-asks a question a local rung just answered, that answer didn't
	// satisfy — mark it as the negative label and force-escalate so they get
	// a fresh LLM answer instead of the same local one again.
	if retryOf = k.detectReAsk(goal, start); retryOf != "" {
		k.log("cascade", "Re-ask of a question rung ("+retryOf+") just answered — escalating to the LLM for a fresh answer")
		return finish(router.RungLLM, "llm", core.Confidence{
			Reason: "user re-asked a locally-answered question; local answer rejected",
		}, "", nil)
	}

	// Rung 0 — deterministic tools (structural questions, exact answers).
	if entryRung <= router.RungDeterministic {
		if ans, conf, ok := k.tryDeterministicRung(ctx, goal); ok && conf.Score >= k.rungThreshold(router.RungDeterministic) {
			return finish(router.RungDeterministic, "deterministic", conf, ans, nil)
		}
	}

	// Rung 1 — answer cache (exact / strict near-duplicate of a past
	// successful no-tool task). Subsumes the previous Step 3.02
	// ConfidentRecall check in Execute.
	if entryRung <= router.RungCache && k.retriever != nil {
		if ans, ok := k.retriever.ConfidentRecall(goal, 0); ok {
			conf := core.Confidence{
				Score:      0.9,
				Reason:     "exact or ≥0.85 token-Jaccard match to a prior successful no-tool answer",
				Provenance: []string{"episodic-cache"},
			}
			if conf.Score >= k.rungThreshold(router.RungCache) {
				return finish(router.RungCache, "cache", conf, ans, nil)
			}
		}
	}

	// Rung 2 — knowledge-graph query (typed, provenance-carrying facts).
	// SourceNodeIDs (nil for AST-verified code-index answers) is threaded
	// through to the cascade log so a later re-ask can demote the exact
	// episodic-sourced fact(s) that answered, not just the whole rung.
	if entryRung <= router.RungGraph && k.memory != nil {
		if ga, ok := memory.AnswerFromGraph(k.memory.KG(), goal); ok && ga.Confidence.Score >= k.rungThreshold(router.RungGraph) {
			return finish(router.RungGraph, "graph", ga.Confidence, ga.Text, ga.SourceNodeIDs)
		}
	}

	// Rung 3 — confident episodic recall (memory/recall_answer.go): graded
	// reuse of ONE specific past successful answer, matched on the answer
	// text (with deterministic acronym bridging) or by embedding cosine.
	// Unlike rungs 0-2 it has no KG node IDs to demote, so a rejected answer
	// is corrected by the rung-wide re-ask mechanism alone (threshold raise
	// + forced escalation). Sub-threshold matches stay context injection.
	if entryRung <= router.RungRecall && k.retriever != nil {
		if ra, ok := k.retriever.BestRecallAnswer(goal, recallAnswerToolMaxAge); ok && ra.Score >= k.rungThreshold(router.RungRecall) {
			conf := core.Confidence{
				Score:      ra.Score,
				Reason:     ra.Reason,
				Provenance: []string{"episodic:" + ra.ID},
			}
			return finish(router.RungRecall, "recall", conf, formatRecallAnswer(ra), nil)
		}
	}

	// Escalate: rungs 4/5 (the existing LLM execution paths) take over.
	return finish(router.RungLLM, "llm", core.Confidence{
		Reason: "no retrieval rung reached the confidence threshold",
	}, "", nil)
}

// formatRecallAnswer cites the served answer's origin so a replayed memory is
// never mistaken for a fresh lookup, and names the escape hatch — immediately
// re-asking IS the designed correction path (detectReAsk force-escalates and
// counts the negative label).
func formatRecallAnswer(ra *memory.RecallAnswer) string {
	src := "a previous answer"
	if len(ra.ToolsUsed) > 0 {
		src += " (via " + strings.Join(ra.ToolsUsed, ", ") + ")"
	}
	return fmt.Sprintf("%s\n\n(From %s to %q on %s — ask again to re-check with the model.)",
		ra.Output, src, strutil.Truncate(ra.Goal, 80), ra.Timestamp.Format("2006-01-02"))
}

// ============================================================================
// RUNG 0 — deterministic tool dispatch
// ============================================================================

// Structural question forms rung 0 can serve. Captured identifiers are
// cleaned and validated before dispatch; on any parse failure rung 0 simply
// misses (binary confidence — it either resolves or it doesn't).
var (
	reCascadeDefine = regexp.MustCompile(`(?i)\b(?:where\s+is|where's)\s+([\w.]+)\s+(?:defined|declared|implemented)|\b(?:go\s+to|find|show)\s+(?:the\s+)?definition\s+of\s+([\w.]+)`)
	reCascadeRefs   = regexp.MustCompile(`(?i)\b(?:who|what)\s+(?:calls|references|uses)\s+([\w.]+)|\bfind\s+(?:all\s+)?(?:references|refs)\s+(?:to|of|for)\s+([\w.]+)`)
	reCascadeDeps   = regexp.MustCompile(`(?i)\b(?:who|which\s+files?|what)\s+imports?\s+([\w./-]+)|\b(?:dependencies|dependents)\s+of\s+([\w./-]+)|\bwhat\s+depends\s+on\s+([\w./-]+)`)
)

// tryDeterministicRung maps a structural question onto one of the READ-ONLY
// deterministic tools (definitions/references/dependencies — never rename)
// and runs it through the registry so the circuit breaker still applies.
func (k *Kernel) tryDeterministicRung(ctx context.Context, goal string) (string, core.Confidence, bool) {
	if k.registry == nil {
		return "", core.Confidence{}, false
	}

	tool, args := "", map[string]interface{}{}
	switch {
	case matchCapture(reCascadeDefine, goal) != "":
		sym := matchCapture(reCascadeDefine, goal)
		// Receiver-qualified names ("Router.Route") resolve by method name.
		if i := strings.LastIndex(sym, "."); i > 0 {
			sym = sym[i+1:]
		}
		tool, args = "deterministic_definitions", map[string]interface{}{"symbol": sym}
	case matchCapture(reCascadeRefs, goal) != "":
		tool, args = "deterministic_references", map[string]interface{}{"symbol": matchCapture(reCascadeRefs, goal)}
	case matchCapture(reCascadeDeps, goal) != "":
		tool, args = "deterministic_dependencies", map[string]interface{}{"package": matchCapture(reCascadeDeps, goal)}
	default:
		return "", core.Confidence{}, false
	}

	res, err := k.registry.Execute(ctx, tool, args)
	if err != nil || res == nil || !res.Success {
		return "", core.Confidence{}, false
	}
	// The deterministic tools report "found nothing" as Success=true with a
	// "No ..." message — that is a miss for cascade purposes, not an answer.
	if strings.HasPrefix(strings.TrimSpace(res.Output), "No ") {
		return "", core.Confidence{}, false
	}
	conf := core.Confidence{
		Score:      1.0,
		Reason:     "resolved by " + tool + " (AST/ripgrep, exact)",
		Provenance: []string{tool},
	}
	return res.Output, conf, true
}

// matchCapture returns the first non-empty capture group of re in s, cleaned
// of trailing punctuation/quotes, or "".
func matchCapture(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	for _, g := range m[1:] {
		if g != "" {
			return strings.Trim(g, `"'?.!`)
		}
	}
	return ""
}

// itoaCascade renders a small rung index without strconv (matching the
// deterministic package's zero-dep style is unnecessary here, but the rung
// range is 0–5 so a table is simplest).
func itoaCascade(n int) string {
	if n >= 0 && n <= 9 {
		return string(rune('0' + n))
	}
	return "?"
}
