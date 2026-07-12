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
