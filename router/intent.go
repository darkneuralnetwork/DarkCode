package router

import "strings"

// IsGeneralQuestion reports whether a query is clearly a conversational
// question/explanation that needs no tools or file access. It lets callers
// route obvious general questions to a single-model-call path instead of the
// tool pipeline (classifier + tools + multiple LLM calls).
//
// It is deliberately conservative: it only returns true when the query clearly
// reads as a question AND carries no imperative build verb or file/path signal.
// Anything ambiguous returns false so tool-worthy requests are unaffected.
//
// It lives in router (not server) so both the HTTP chat handler and the
// orchestrator kernel — which serves the CLI — share one definition.
func IsGeneralQuestion(query string) bool {
	s := strings.ToLower(strings.TrimSpace(query))
	if s == "" {
		return false
	}

	// Imperative action verbs at the START imply a task that touches tools/
	// files → not a plain question.
	firstWord := s
	if i := strings.IndexByte(s, ' '); i > 0 {
		firstWord = s[:i]
	}
	if intentActionVerbs[strings.TrimRight(firstWord, ".,:;!?")] {
		return false
	}

	// File / path / codebase signals imply a tool task.
	if strings.Contains(s, "/") || intentContainsAny(s, intentFileSignals) || intentHasCodeExtension(s) {
		return false
	}

	// Clear question shape → general.
	if strings.HasSuffix(s, "?") {
		return true
	}
	for _, lead := range intentQuestionLeads {
		if strings.HasPrefix(s, lead) {
			return true
		}
	}
	return false
}

var intentActionVerbs = map[string]bool{
	"build": true, "create": true, "implement": true, "fix": true,
	"refactor": true, "add": true, "write": true, "run": true,
	"install": true, "make": true, "generate": true, "deploy": true,
	"delete": true, "remove": true, "rename": true, "edit": true,
	"update": true, "modify": true, "patch": true, "migrate": true,
	"scaffold": true, "debug": true, "setup": true, "configure": true,
	"compile": true, "test": true, "clone": true, "download": true,
	"commit": true, "push": true, "pull": true, "open": true,
}

var intentFileSignals = []string{
	"the file", "this file", "the repo", "this repo", "the codebase",
	"this codebase", "the project", "this project", "the directory",
	"list files", "read the", "open the",
}

var intentQuestionLeads = []string{
	"what ", "why ", "how ", "who ", "when ", "where ", "which ",
	"is ", "are ", "can you explain", "explain ", "describe ",
	"define ", "tell me", "difference between", "compare ",
	"summarize ", "summarise ", "does ", "do ", "should ", "could ",
}

var intentCodeExtensions = []string{
	".go", ".js", ".ts", ".tsx", ".jsx", ".py", ".rs", ".java",
	".c", ".cpp", ".h", ".rb", ".php", ".html", ".css", ".json",
	".yaml", ".yml", ".md", ".sh", ".sql",
}

func intentHasCodeExtension(s string) bool {
	for _, ext := range intentCodeExtensions {
		if strings.Contains(s, ext) {
			return true
		}
	}
	return false
}

func intentContainsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
