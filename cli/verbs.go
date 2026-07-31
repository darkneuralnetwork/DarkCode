package cli

// verbs.go — choosing an execution strategy at the moment you need it.
//
// Strategy used to live in configuration: a persistent agentic_loop flag, a
// max_loops number, a routing mode. But whether a request should iterate
// depends on whether THAT request is multi-step, which the task knows and the
// installation cannot. Asking for it once, globally, means predicting something
// that changes every message.
//
// The shape of the old mistake is preserved in what this replaces. Typing
// `/chatmode loop` used to answer:
//
//	(enable the Agentic Loop in /config for Loop mode to take effect)
//
// — a command surface apologising for a configuration surface. Two places
// expressing one intent, with a third piece of code to reconcile them.
//
// So a verb is one shot by default. `/loop <task>` applies to that message and
// nothing else; `/mode loop` is available for anyone who genuinely wants it to
// stick, because a sticky mode is how people end up in the wrong one without
// noticing — which is exactly what a persistent flag already was.

import (
	"strings"
)

// strategy is the bundle a verb selects. A verb sets several things at once on
// purpose: `/graph` meaning "plan, then approve, then run the waves, then
// prove each node" is one decision, and splitting it back into four settings is
// how it became configuration in the first place.
type strategy struct {
	name  string
	loop  string // ApplyRequestOverrides loop:  "on" | "off" | ""
	tools string // ApplyRequestOverrides tools: "on" | "readonly" | ""
	mode  string // ApplyRequestOverrides mode:  routing mode, "" to leave alone
	plan  string // ApplyPlanOverride: "always" | "never" | "" (adaptive)
	help  string
}

var strategies = map[string]strategy{
	"ask": {
		name: "ask", loop: "off", tools: "readonly",
		help: "answer from the project without changing anything",
	},
	"loop": {
		name: "loop", loop: "on", tools: "on", plan: "never",
		help: "iterate on the task directly until the checks pass",
	},
	"graph": {
		// The difference from /loop is the plan. Both iterate; /graph always
		// decomposes first, so there are per-task acceptance criteria to prove
		// and a Blueprint worth looking at. Without forcing this the two verbs
		// selected identical behaviour and /graph was a synonym.
		name: "graph", loop: "on", tools: "on", plan: "always",
		help: "plan the work, run it as a task graph, prove each task",
	},
	"consensus": {
		name: "consensus", loop: "off", tools: "on", mode: "consensus",
		help: "answer with every registered model, then synthesise",
	},
}

// StrategyNames lists the verbs, for help text and completion.
func StrategyNames() []string { return []string{"ask", "loop", "graph", "consensus"} }

// splitVerb recognises a leading strategy verb on an input line and returns the
// strategy plus the remaining text.
//
// A bare verb with no task ("/loop") is NOT a strategy selection — it is a user
// asking what the verb does, and answering it with silence while quietly arming
// a mode would be the sticky-mode trap again. The caller shows help instead.
func splitVerb(line string) (strategy, string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "/") {
		return strategy{}, "", false
	}
	rest := s[1:]
	word := rest
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		word = rest[:i]
		rest = strings.TrimSpace(rest[i:])
	} else {
		rest = ""
	}
	st, ok := strategies[strings.ToLower(word)]
	if !ok || rest == "" {
		return strategy{}, "", false
	}
	return st, rest, true
}

// verbHelp renders the one-line description of every verb.
func verbHelp() string {
	var b strings.Builder
	b.WriteString("Strategy verbs — each applies to that one message:\n")
	for _, n := range StrategyNames() {
		s := strategies[n]
		b.WriteString("  /" + padRight(s.name, 11) + s.help + "\n")
	}
	b.WriteString("\n  /always <verb> keep using it until you say otherwise\n")
	b.WriteString("  /loop until `go test ./...` passes: <task>\n" +
		"                 stop when that command actually passes, not when the model says so\n")
	return b.String()
}
