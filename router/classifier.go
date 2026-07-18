package router

// classifier.go — TaskClassifier categorizes incoming requests by type so the
// router can pick the right execution mode. Previously this had only 2
// keyword checks (rename/find references → deterministic; explain/review →
// selective). It now covers a broader vocabulary and structural patterns.

import "strings"

// TaskType categorizes the incoming user request.
type TaskType string

const (
	TaskTypeDeterministic TaskType = "deterministic" // no LLM needed (rename, refs, imports)
	TaskTypeSelective     TaskType = "selective"     // single fast model (explain, summarize)
	TaskTypeFullConsensus TaskType = "full_consensus" // multi-model reasoning (design, debug)
	TaskTypeTinyLocal     TaskType = "tiny_local"     // run on tiny local model
	TaskTypeMediumLocal   TaskType = "medium_local"   // run on medium local model
	TaskTypeMediumLocalCoding TaskType = "medium_local_coding" // run on medium local model with coding LoRA
)

type TaskClassifier struct{}

func NewTaskClassifier() *TaskClassifier {
	return &TaskClassifier{}
}

// Classify determines the nature of the task based on keywords and heuristics.
func (c *TaskClassifier) Classify(query string) TaskType {
	q := strings.ToLower(query)

	// Deterministic: structural code operations that should never use an LLM.
	detKeywords := []string{
		"rename", "find references", "find refs", "go to definition",
		"find definition", "list imports", "show imports",
		"dependency graph", "dependencies of", "who imports",
		"call graph", "who calls",
	}
	for _, kw := range detKeywords {
		if strings.Contains(q, kw) {
			return TaskTypeDeterministic
		}
	}

	// Full consensus: complex reasoning / design / architecture.
	consensusKeywords := []string{
		"design", "architect", "refactor", "debug", "why does",
		"root cause", "investigate", "analyze", "analyse",
		"plan", "strategy", "evaluate", "compare", "trade-off",
		"tradeoff", "which approach", "best practice",
	}
	for _, kw := range consensusKeywords {
		if strings.Contains(q, kw) {
			return TaskTypeFullConsensus
		}
	}

	// Specific Local Model assignments
	if strings.Contains(q, "explain error") || strings.Contains(q, "explain this error") {
		return TaskTypeTinyLocal
	}
	if strings.Contains(q, "review code") || strings.Contains(q, "review this code") {
		return TaskTypeMediumLocal
	}
	codingKeywords := []string{"write code", "implement", "function", "create class", "script", "code snippet"}
	for _, kw := range codingKeywords {
		if strings.Contains(q, kw) {
			return TaskTypeMediumLocalCoding
		}
	}

	// Selective: single-pass tasks.
	selectiveKeywords := []string{
		"explain", "review", "summarize", "summarise", "describe",
		"what is", "what does", "how does", "show me",
		"write a test", "generate a comment", "document",
		"translate", "convert", "format",
	}
	for _, kw := range selectiveKeywords {
		if strings.Contains(q, kw) {
			return TaskTypeSelective
		}
	}

	// Default: if the query is long, treat it as consensus-worthy.
	if len(query) > 200 {
		return TaskTypeFullConsensus
	}

	return TaskTypeSelective
}

// ============================================================================
// ENTRY-POINT CLASSIFIER (local-first upgrade §2, Phase A)
//
// Maps a query to the cascade rung it should ENTER at, so a rigid waterfall
// doesn't waste latency probing rungs that obviously can't answer:
//
//	rung 0 — deterministic tools (structural/code-navigation intent)
//	rung 1 — answer cache / near-duplicate recall ("did we", cheap default)
//	rung 2 — knowledge-graph query (relational/factual: imports, refs, tools)
//	rung 4 — LLM synthesis (explain/why/design/action — skip retrieval rungs)
//
// A query entering at rung N still escalates upward normally on low
// confidence; the entry point only skips rungs BELOW it.
// ============================================================================

// Cascade rung indices (§2 of the upgrade plan). Rung 3 (ranked recall) is
// context injection rather than a direct answerer, so EntryRung never returns
// it; rung 5 (cloud) is a routing decision inside the LLM path.
const (
	RungDeterministic = 0
	RungCache         = 1
	RungGraph         = 2
	RungRecall        = 3
	RungLLM           = 4
	RungCloud         = 5
)

// EntryRung picks the cascade entry rung for a query.
func (c *TaskClassifier) EntryRung(query string) int {
	q := strings.ToLower(query)

	// Structural/code-navigation → deterministic tools first.
	structuralKeywords := []string{
		"rename", "find references", "find refs", "go to definition",
		"find definition", "where is", "where's", "defined", "declared",
		"list imports", "show imports", "who imports", "which files import",
		"dependency graph", "dependencies of", "depends on",
		"call graph", "who calls", "who references", "who uses",
	}
	for _, kw := range structuralKeywords {
		if strings.Contains(q, kw) {
			return RungDeterministic
		}
	}

	// Relational/history → knowledge-graph query.
	graphKeywords := []string{
		"related to", "what tools", "which tools", "what is connected",
		"what did we use",
	}
	for _, kw := range graphKeywords {
		if strings.Contains(q, kw) {
			return RungGraph
		}
	}

	// Explicit memory intent → cache/recall rungs.
	recallKeywords := []string{
		"did we", "have we", "last time", "what did we", "previously",
		"again", "before",
	}
	for _, kw := range recallKeywords {
		if strings.Contains(q, kw) {
			return RungCache
		}
	}

	// Question-shaped queries (leading interrogative/explanatory verb) are
	// knowledge requests even when they mention action verbs later ("explain
	// how to implement X" is a question, not an implementation request) —
	// they stay cache-eligible below.
	questionLeads := []string{
		"explain", "describe", "summarize", "summarise", "compare",
		"what", "how", "why", "who", "where", "when", "which",
		"does", "do ", "is ", "are ", "can i", "should",
	}
	isQuestion := false
	for _, lead := range questionLeads {
		if strings.HasPrefix(q, lead) {
			isQuestion = true
			break
		}
	}

	// Action intent (side effects or a novel artifact expected) → go straight
	// to the LLM/tool path. These must NEVER be served from a cached past
	// answer: filesystem state may differ and the user expects the action to
	// actually run, not a replayed claim that it did.
	if !isQuestion {
		actionKeywords := []string{
			"refactor", "implement", "create", "write", "build", "fix",
			"add ", "remove ", "delete", "update", "generate", "install",
			"deploy", "commit", "run ",
		}
		for _, kw := range actionKeywords {
			if strings.Contains(q, kw) {
				return RungLLM
			}
		}
	}

	// Default (including explain/why/design questions): the cache rung.
	// ConfidentRecall is a ~free strict lookup over past successful no-tool
	// answers — a repeated explanation question is its single biggest win,
	// and this preserves the pre-cascade behavior where it ran for every
	// query. Rungs 0/2 are skipped (their patterns can't match these).
	return RungCache
}
