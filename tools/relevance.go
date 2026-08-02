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

// domainTools maps a narrow tool to the words that make it relevant. A goal has
// to actually mention the domain; inferring "they might want a PDF" from
// silence is how the saving disappears.
//
// This list used to hold eighteen tools. It holds four, and the difference is
// the whole point of the redesign.
//
// Measured against the registry, the four below are 3,010 of 8,171 schema bytes
// — 37% of the per-turn cost, concentrated in tools that a coding task genuinely
// never needs unless it says so. Gating them is most of the saving available.
//
// The other fourteen were gated on words the USER had to type, which inverts
// who is supposed to know. "fix the failing test" mentions no graph, no symbol
// and no breakpoint, so it was offered no graph_query, no lsp and no debug —
// the three tools that most distinguish this agent from a plain ReAct loop,
// withheld from exactly the task they were built for. The unlock-on-request
// escape could not save it either: a model cannot ask for a tool it has never
// been shown.
//
// Cutting a further ~2,000 bytes is not worth making the agent worse at its
// core job. Anything not listed here is offered.
var domainTools = map[string][]string{
	"pdf":              {"pdf", "document", "report", "invoice"},
	"image":            {"image", "picture", "screenshot", "png", "jpg", "jpeg", "diagram", "photo"},
	"browser_subagent": {"browser", "navigate", "click", "webpage", "web page", "scrape", "screenshot"},
	"monitoring":       {"cpu", "memory usage", "process", "disk", "load", "health", "monitor", "resource"},
}

// RelevantSchemas returns the schemas worth sending for this goal: everything
// except the narrow tools in domainTools, which are included only when the goal
// mentions their domain or the model has already asked for them this run.
//
// The default is to OFFER. Withholding is the exception and has to be earned by
// a tool being both expensive and genuinely task-specific — see domainTools.
func RelevantSchemas(goal string, all []core.ToolSchema, extra map[string]bool) []core.ToolSchema {
	g := strings.ToLower(goal)
	out := make([]core.ToolSchema, 0, len(all))
	for _, s := range all {
		name := s.Function.Name
		words, narrow := domainTools[name]
		if !narrow || extra[name] {
			out = append(out, s)
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
