package memory

// kg_answer.go — the graph-query answer path (local-first upgrade §7 Phase B,
// cascade rung 2). Answers structural/relational questions by knowledge-graph
// traversal with citations and NO LLM:
//
//   "where is HybridRetriever defined?"       → symbol node → file:line
//   "which files import github.com/x/router?" → imports edges → file list
//   "who references Route?"                   → symbol reference fan-in
//   "what tools did we use for <topic>?"      → task→tool activity edges
//   "how did we fix <problem>?"               → fix node via fixed_by edge
//   "why did we decide <topic>?"              → decision node
//
// Confidence policy (plan §2/§10.5): only *sourced* facts may answer. A
// symbol/import fact carries file:line provenance written by the code index,
// so a hit is high-confidence by construction — the answer points at real
// code the user can verify. Unsourced facts (concept co-occurrence nodes)
// never answer here; they only *suggest* via the existing recall/kgBoost
// context injection. Fix/decision facts (local-first upgrade Phase D,
// orchestrator/reflection.go) are sourced from episodic task outcomes rather
// than AST ground truth, so they answer at a lower confidence than a code
// fact but well above an unsourced concept edge — see answerFixHistory/
// answerDecisionHistory.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/darkcode/core"
)

// GraphAnswer is a rung-2 result: a citation-backed textual answer plus the
// comparable confidence signal the cascade gates on.
type GraphAnswer struct {
	Text       string
	Confidence core.Confidence
	// SourceNodeIDs are the KG node IDs whose own (possibly demoted)
	// Confidence backed this answer — distinct from Confidence.Provenance,
	// which is a human-readable citation list (task IDs / file paths, not
	// necessarily KG node IDs, and asserted verbatim by existing tests).
	// Populated only for episodic-sourced fix/decision answers, which are
	// subject to write-back demotion (local-first upgrade Phase D
	// hardening); nil for AST-verified code-index answers, which aren't —
	// see orchestrator/cascade.go's detectReAsk.
	SourceNodeIDs []string
}

// Question patterns, compiled once. Identifiers are captured liberally and
// validated afterwards (must look like a code identifier / import path).
var (
	reDefined         = regexp.MustCompile(`(?i)\b(?:where\s+is|where's)\s+([\w.]+)\s+(?:defined|declared|implemented)\b|(?i)\b(?:definition|declaration)\s+of\s+([\w.]+)`)
	reImports         = regexp.MustCompile(`(?i)\b(?:which\s+files?|who|what)\s+(?:imports?|depends?\s+on)\s+([\w./-]+)`)
	reRefs            = regexp.MustCompile(`(?i)\b(?:who|what|how\s+many\s+(?:files?|places?|sites?))\s+(?:references?|uses?|calls?)\s+([\w.]+)`)
	reTools           = regexp.MustCompile(`(?i)\bwhat\s+tools?\s+(?:were\s+used|did\s+we\s+use|used|solved|helped)\b`)
	reFixHistory      = regexp.MustCompile(`(?i)\b(?:how\s+did\s+we\s+fix|have\s+we\s+fixed|has\s+(?:this|it)\s+been\s+fixed|what\s+fixed|is\s+there\s+a\s+(?:known\s+)?fix\s+for|fix(?:ed)?\s+for)\b`)
	reDecisionHistory = regexp.MustCompile(`(?i)\b(?:why\s+did\s+we\s+decide|what\s+did\s+we\s+decide|what\s+was\s+the\s+decision|what's\s+the\s+decision)\b`)
)

// AnswerFromGraph attempts to answer a structural/relational question from
// the knowledge graph alone. Returns (answer, true) on a confident hit and
// (nil, false) when the question isn't graph-shaped or the graph has no
// sourced fact for it — the cascade then escalates to the next rung.
func AnswerFromGraph(kg core.KnowledgeGraphStore, query string) (*GraphAnswer, bool) {
	if kg == nil || strings.TrimSpace(query) == "" {
		return nil, false
	}

	if m := reDefined.FindStringSubmatch(query); m != nil {
		sym := firstNonEmpty(m[1], m[2])
		return answerDefinition(kg, sym)
	}
	if m := reImports.FindStringSubmatch(query); m != nil {
		return answerImporters(kg, m[1])
	}
	if m := reRefs.FindStringSubmatch(query); m != nil {
		return answerReferences(kg, m[1])
	}
	if reTools.MatchString(query) {
		return answerToolsForTopic(kg, query)
	}
	if reFixHistory.MatchString(query) {
		return answerFixHistory(kg, query)
	}
	if reDecisionHistory.MatchString(query) {
		return answerDecisionHistory(kg, query)
	}
	return nil, false
}

// answerDefinition resolves "where is X defined" from symbol nodes. Accepts
// plain names ("Route") and receiver-qualified names ("Router.Route").
func answerDefinition(kg core.KnowledgeGraphStore, symbol string) (*GraphAnswer, bool) {
	name, recv := symbol, ""
	if i := strings.LastIndex(symbol, "."); i > 0 {
		recv, name = symbol[:i], symbol[i+1:]
	}
	if !isIdentifier(name) {
		return nil, false
	}

	var matches []*core.KGNode
	for _, n := range kg.FindByType(core.KGNodeSymbol) {
		if !strings.EqualFold(n.Label, name) || n.Provenance == "" {
			continue
		}
		if recv != "" && !strings.Contains(strings.TrimPrefix(n.Properties["receiver"], "*"), recv) {
			continue
		}
		matches = append(matches, n)
	}
	if len(matches) == 0 {
		return nil, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Provenance < matches[j].Provenance })

	var sb strings.Builder
	var prov []string
	fmt.Fprintf(&sb, "Found %d declaration(s) of %q (from the code knowledge graph):\n", len(matches), symbol)
	for _, n := range matches {
		kind := n.Properties["kind"]
		if r := n.Properties["receiver"]; r != "" {
			fmt.Fprintf(&sb, "  %s (method on %s) — %s\n", n.Label, r, n.Provenance)
		} else {
			fmt.Fprintf(&sb, "  %s %s — %s\n", kind, n.Label, n.Provenance)
		}
		prov = append(prov, n.Provenance)
	}
	sb.WriteString("(Answered from the indexed knowledge graph; re-run deterministic_kg_sync if the code changed recently.)")
	return &GraphAnswer{
		Text: sb.String(),
		Confidence: core.Confidence{
			Score:      0.95,
			Reason:     "symbol fact with file:line provenance from the deterministic code index",
			Provenance: prov,
		},
	}, true
}

// answerImporters resolves "which files import P" from imports edges. The
// package may be given as a full import path or a trailing fragment
// ("router" matches "github.com/darkcode/router").
func answerImporters(kg core.KnowledgeGraphStore, pkg string) (*GraphAnswer, bool) {
	pkg = strings.Trim(pkg, `"'?.`)
	if pkg == "" {
		return nil, false
	}
	var target *core.KGNode
	for _, n := range kg.FindByType(core.KGNodePackage) {
		if strings.EqualFold(n.Label, pkg) || strings.HasSuffix(strings.ToLower(n.Label), "/"+strings.ToLower(pkg)) {
			target = n
			break
		}
	}
	if target == nil {
		return nil, false
	}
	var files []string
	for _, e := range kg.GetEdges(target.ID) {
		if e.Relation == core.KGRelImports && e.To == target.ID {
			files = append(files, strings.TrimPrefix(e.From, "file:"))
		}
	}
	if len(files) == 0 {
		return nil, false
	}
	sort.Strings(files)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d file(s) import %s (from the code knowledge graph):\n", len(files), target.Label)
	for _, f := range files {
		fmt.Fprintf(&sb, "  %s\n", f)
	}
	return &GraphAnswer{
		Text: sb.String(),
		Confidence: core.Confidence{
			Score:      0.9,
			Reason:     "import facts recorded by the deterministic code index",
			Provenance: files,
		},
	}, true
}

// answerReferences resolves "who references X" from the symbol's indexed
// reference fan-in. Aggregate only — per-site citations are rung 0's job
// (deterministic_references), which the answer points at.
func answerReferences(kg core.KnowledgeGraphStore, symbol string) (*GraphAnswer, bool) {
	if !isIdentifier(strings.TrimSuffix(symbol, "?")) {
		return nil, false
	}
	symbol = strings.TrimSuffix(symbol, "?")
	var matches []*core.KGNode
	for _, n := range kg.FindByType(core.KGNodeSymbol) {
		if strings.EqualFold(n.Label, symbol) && n.Provenance != "" {
			matches = append(matches, n)
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	var sb strings.Builder
	var prov []string
	for _, n := range matches {
		refs := n.Properties["references"]
		if refs == "" {
			refs = "0"
		}
		fmt.Fprintf(&sb, "%s (defined at %s) is referenced by %s other file(s).\n", n.Label, n.Provenance, refs)
		prov = append(prov, n.Provenance)
	}
	sb.WriteString("For exact reference sites run the deterministic_references tool.")
	return &GraphAnswer{
		Text: sb.String(),
		Confidence: core.Confidence{
			Score:      0.85,
			Reason:     "reference fan-in counted by the deterministic code index",
			Provenance: prov,
		},
	}, true
}

// answerToolsForTopic resolves "what tools did we use for <topic>" from task
// activity facts (task nodes linked to tool nodes by the memory recorder).
// The topic is matched by token overlap against task labels.
func answerToolsForTopic(kg core.KnowledgeGraphStore, query string) (*GraphAnswer, bool) {
	qTokens := tokenize(query)
	if len(qTokens) == 0 {
		return nil, false
	}
	qset := make(map[string]bool, len(qTokens))
	for _, t := range qTokens {
		qset[t] = true
	}
	// noise tokens from the question form itself
	for _, t := range []string{"tools", "tool", "used", "solved", "helped", "tasks", "task"} {
		delete(qset, t)
	}
	if len(qset) == 0 {
		return nil, false
	}

	toolsByTask := make(map[string][]string)
	for _, task := range kg.FindByType(core.KGNodeTask) {
		overlap := 0
		for _, t := range tokenize(task.Label) {
			if qset[t] {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		for _, e := range kg.GetEdges(task.ID) {
			if e.Relation != core.KGRelUsedBy && e.Relation != core.KGRelUsedTool {
				continue
			}
			other := e.To
			if other == task.ID {
				other = e.From
			}
			if strings.HasPrefix(other, "tool:") {
				toolsByTask[task.Label] = append(toolsByTask[task.Label], strings.TrimPrefix(other, "tool:"))
			}
		}
	}
	if len(toolsByTask) == 0 {
		return nil, false
	}
	var taskLabels []string
	for label := range toolsByTask {
		taskLabels = append(taskLabels, label)
	}
	sort.Strings(taskLabels)
	var sb strings.Builder
	sb.WriteString("From past task activity in the knowledge graph:\n")
	for _, label := range taskLabels {
		tools := dedupStrings(toolsByTask[label])
		fmt.Fprintf(&sb, "  %q used: %s\n", label, strings.Join(tools, ", "))
	}
	return &GraphAnswer{
		Text: sb.String(),
		Confidence: core.Confidence{
			Score:      0.8,
			Reason:     "task→tool activity facts recorded from real executions",
			Provenance: taskLabels,
		},
	}, true
}

// fixHistoryNoiseWords / decisionNoiseWords strip the question's own verbs so
// topic matching scores the subject, not the question form.
var fixHistoryNoiseWords = []string{
	"how", "did", "we", "fix", "fixed", "have", "has", "this", "been",
	"before", "what", "for", "there", "known",
}
var decisionNoiseWords = []string{
	"why", "did", "we", "decide", "decision", "what", "was", "the", "about",
}

// defaultFactConfidence is the fallback score for an episodic-sourced fact
// whose stored Confidence is zero (unset — e.g. a node written before Phase
// D's write-back hardening added graded confidence, or by any future writer
// that forgets to set it). Real fix/decision facts carry their own graded
// Confidence (orchestrator/memory_recorder.go's fixFactConfidence/
// decisionFactConfidence) which answerFixHistory/answerDecisionHistory read
// directly — this is only the legacy-data safety net, kept comfortably above
// the cascade's default 0.75 answer threshold.
const defaultFactConfidence = 0.78

// factNodeConfidence returns n's own Confidence, or defaultFactConfidence if
// it was never set (zero).
func factNodeConfidence(n *core.KGNode) float64 {
	if n.Confidence <= 0 {
		return defaultFactConfidence
	}
	return n.Confidence
}

// minConfidence returns the lowest value in scores. Used to aggregate
// multiple matched facts into one answer confidence — conservative by
// design: one weak or demoted fact must not be masked by averaging it with
// a stronger one (write-back governance, local-first upgrade Phase D
// hardening).
func minConfidence(scores []float64) float64 {
	min := scores[0]
	for _, s := range scores[1:] {
		if s < min {
			min = s
		}
	}
	return min
}

// answerFixHistory resolves "how did we fix X" / "has X been fixed before"
// from fix facts (local-first upgrade Phase D: orchestrator/reflection.go
// promotes a task that resolved a matched prior failure into a KGNodeFix
// linked to the problem task by KGRelFixedBy). Unlike the code-index answers
// above, these facts are sourced from episodic outcomes, not AST ground
// truth — scored lower accordingly but still well above an unsourced concept
// edge, and still cited (task provenance, not file:line).
func answerFixHistory(kg core.KnowledgeGraphStore, query string) (*GraphAnswer, bool) {
	qset := queryTopicSet(query, fixHistoryNoiseWords)
	if len(qset) == 0 {
		return nil, false
	}

	type fixMatch struct {
		problem *core.KGNode
		fix     *core.KGNode
	}
	var matches []fixMatch
	for _, e := range kg.AllEdges() {
		if e.Relation != core.KGRelFixedBy {
			continue
		}
		problem, ok := kg.GetNode(e.From)
		if !ok {
			continue
		}
		fix, ok := kg.GetNode(e.To)
		if !ok || fix.Provenance == "" {
			continue
		}
		text := problem.Label + " " + fix.Label
		overlap := 0
		for _, t := range tokenize(text) {
			if qset[t] {
				overlap++
			}
		}
		if overlap > 0 {
			matches = append(matches, fixMatch{problem: problem, fix: fix})
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].problem.Label < matches[j].problem.Label })

	var sb strings.Builder
	var prov, nodeIDs []string
	var scores []float64
	sb.WriteString("From past task activity in the knowledge graph:\n")
	for _, m := range matches {
		resolution := m.fix.Properties["resolution"]
		fmt.Fprintf(&sb, "  Problem %q was fixed: %s (from task %s)\n", m.problem.Label, strings.TrimSpace(resolution), m.fix.Provenance)
		prov = append(prov, m.fix.Provenance)
		nodeIDs = append(nodeIDs, m.fix.ID)
		scores = append(scores, factNodeConfidence(m.fix))
	}
	return &GraphAnswer{
		Text: sb.String(),
		Confidence: core.Confidence{
			Score:      minConfidence(scores),
			Reason:     "fix fact derived from a matched prior-failure→success episodic pair, not code-verified",
			Provenance: prov,
		},
		SourceNodeIDs: nodeIDs,
	}, true
}

// answerDecisionHistory resolves "why did we decide X" / "what did we decide
// about X" from decision facts (Phase D: a design/architecture-flavored
// successful task is promoted to a KGNodeDecision with its reflection
// lessons as rationale).
func answerDecisionHistory(kg core.KnowledgeGraphStore, query string) (*GraphAnswer, bool) {
	qset := queryTopicSet(query, decisionNoiseWords)
	if len(qset) == 0 {
		return nil, false
	}

	var matches []*core.KGNode
	for _, n := range kg.FindByType(core.KGNodeDecision) {
		if n.Provenance == "" {
			continue
		}
		overlap := 0
		for _, t := range tokenize(n.Label) {
			if qset[t] {
				overlap++
			}
		}
		if overlap > 0 {
			matches = append(matches, n)
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Label < matches[j].Label })

	var sb strings.Builder
	var prov, nodeIDs []string
	var scores []float64
	sb.WriteString("From past decisions recorded in the knowledge graph:\n")
	for _, n := range matches {
		fmt.Fprintf(&sb, "  %q — %s (from task %s)\n", n.Label, n.Properties["rationale"], n.Provenance)
		prov = append(prov, n.Provenance)
		nodeIDs = append(nodeIDs, n.ID)
		scores = append(scores, factNodeConfidence(n))
	}
	return &GraphAnswer{
		Text: sb.String(),
		Confidence: core.Confidence{
			Score:      minConfidence(scores),
			Reason:     "decision fact derived from episodic task outcome, not code-verified",
			Provenance: prov,
		},
		SourceNodeIDs: nodeIDs,
	}, true
}

// queryTopicSet tokenizes query and strips noise words, returning the
// remaining topic tokens as a set. Returns nil (not answerable) when nothing
// remains — a bare "have we fixed this before?" with no resolvable subject.
func queryTopicSet(query string, noise []string) map[string]bool {
	qset := make(map[string]bool)
	for _, t := range tokenize(query) {
		qset[t] = true
	}
	for _, w := range noise {
		delete(qset, w)
	}
	return qset
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isIdentifier reports whether s looks like a code identifier.
func isIdentifier(s string) bool {
	return identRe.MatchString(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
