package tools

// relevance.go — not paying to describe tools the task cannot use.
//
// Every tool-enabled model call carries the whole registry as JSON Schema:
// 8,188 bytes, about 2,047 tokens, on EVERY iteration. A 25-turn run spends
// roughly 51,000 tokens describing tools before any conversation happens. It is
// the largest single cost in the system and nothing else is close.
//
// The obvious fix — shortening descriptions — is the wrong one. Measured, the
// descriptions are 2,084 bytes and the JSON Schema parameter blocks are 5,509.
// Trimming prose attacks a quarter of the problem while making the tools harder
// for the model to use correctly, which costs turns.
//
// What actually costs is sending `pdf`, `image`, `browser_subagent` and
// `monitoring` — around 3,000 bytes between them — to a task about renaming a
// Go function. So the filter is on relevance, not on verbosity.
//
// This is a COST optimisation, not a security boundary, and the difference
// decides how it fails. Role scoping (toolscope.go) refuses at dispatch,
// because a research agent must never run a shell whatever it asks for. This
// does the opposite: a tool left out and then asked for is still allowed to
// run, and joins the offered set for the rest of the run. A wrong guess here
// costs one turn; enforcing it would cost the task.

import (
	"strings"

	"github.com/darkcode/core"
)

// coreTools are offered for every task. They are the ones a coding agent
// reaches for without the goal having to hint at them, and withholding one to
// save bytes would trade a certain cost for an uncertain failure.
var coreTools = map[string]bool{
	"read_file": true, "write_file": true, "list_dir": true, "list_files": true,
	"search_files": true, "terminal": true, "patch": true,
	"replace_file_content": true, "memory": true,
}

// domainTools maps a specialised tool to the words that make it relevant. A
// goal has to actually mention the domain; inferring "they might want a PDF"
// from silence is how the saving disappears.
var domainTools = map[string][]string{
	"pdf":                   {"pdf", "document", "report", "invoice"},
	"image":                 {"image", "picture", "screenshot", "png", "jpg", "jpeg", "diagram", "photo"},
	"browser_subagent":      {"browser", "navigate", "click", "webpage", "web page", "scrape", "screenshot"},
	"monitoring":            {"cpu", "memory usage", "process", "disk", "load", "health", "monitor", "resource"},
	"git":                   {"git", "commit", "branch", "merge", "diff", "stage", "checkout", "rebase"},
	"github":                {"github", "pull request", "pr ", "issue", "repo", "release"},
	"web_search":            {"search", "look up", "find out", "latest", "current", "news", "documentation", "docs"},
	"web_fetch":             {"fetch", "url", "http", "website", "download", "page"},
	"research":              {"research", "investigate", "compare", "survey"},
	"todo":                  {"todo", "task list", "checklist", "plan"},
	"debug":                 {"debug", "breakpoint", "step through", "stack trace"},
	"lsp":                   {"definition", "reference", "symbol", "rename", "hover", "completion"},
	"ingest":                {"ingest", "index", "learn from", "teach"},
	"graph_query":           {"graph", "related to", "depends on", "who calls"},
	"self_heal":             {"heal", "auto-fix", "repair"},
	"rank_patches":          {"patch", "candidate", "rank"},
	"pdf_extract":           {"pdf", "extract"},
	"deterministic_kg_sync": {"index", "sync", "graph"},
}

// RelevantSchemas returns the subset of schemas worth sending for this goal:
// the core set, plus any specialised tool the goal actually mentions, plus
// anything in extra (tools the model has already asked for this run).
//
// A tool with no domain entry is treated as core rather than dropped. Being
// unlisted means nobody has classified it yet, and silently withholding an
// unclassified tool would make adding one a trap.
func RelevantSchemas(goal string, all []core.ToolSchema, extra map[string]bool) []core.ToolSchema {
	g := strings.ToLower(goal)
	out := make([]core.ToolSchema, 0, len(all))
	for _, s := range all {
		name := s.Function.Name
		if coreTools[name] || extra[name] {
			out = append(out, s)
			continue
		}
		words, classified := domainTools[name]
		if !classified {
			out = append(out, s) // unknown tool: err toward offering it
			continue
		}
		for _, w := range words {
			if strings.Contains(g, w) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// SchemaNames is a small helper for logging which tools a turn was offered.
func SchemaNames(s []core.ToolSchema) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Function.Name)
	}
	return out
}
