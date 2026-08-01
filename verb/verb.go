// Package verb defines the execution-strategy verbs and the single table both
// the console and the web UI read.
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
// This package exists because that failure has a second form. The verbs first
// shipped inside the console package, so the same intent had a console spelling
// and no web spelling at all: `/loop` worked in one surface and was sent to the
// model as literal text in the other. A table in one place, read by both, is
// what stops the two drifting again.
package verb

import "strings"

// Strategy is the bundle a verb selects. A verb sets several things at once on
// purpose: `/graph` meaning "plan, then approve, then run the waves, then prove
// each node" is one decision, and splitting it back into four settings is how
// it became configuration in the first place.
type Strategy struct {
	Name   string
	Loop   string // kernel override loop:  "on" | "off" | ""
	Tools  string // kernel override tools: "on" | "readonly" | ""
	Mode   string // kernel override mode:  routing mode, "" to leave alone
	Plan   string // plan override: "always" | "never" | "" (adaptive)
	Debate bool   // force the conflict exchange for this message
	Help   string
}

var table = map[string]Strategy{
	"ask": {
		Name: "ask", Loop: "off", Tools: "readonly",
		Help: "answer from the project without changing anything",
	},
	"loop": {
		Name: "loop", Loop: "on", Tools: "on", Plan: "never",
		Help: "iterate on the task directly until the checks pass",
	},
	"graph": {
		// The difference from /loop is the plan. Both iterate; /graph always
		// decomposes first, so there are per-task acceptance criteria to prove
		// and a Blueprint worth looking at. Without forcing this the two verbs
		// selected identical behaviour and /graph was a synonym.
		Name: "graph", Loop: "on", Tools: "on", Plan: "always",
		Help: "plan the work, run it as a task graph, prove each task",
	},
	"consensus": {
		Name: "consensus", Loop: "off", Tools: "on", Mode: "consensus",
		Help: "answer with every registered model, then synthesise",
	},
	"debate": {
		// Consensus plus one round of the models answering each other. Only
		// worth asking for when the question is genuinely contested — on a
		// metered tier this is the most expensive verb there is.
		Name: "debate", Loop: "off", Tools: "on", Mode: "consensus", Debate: true,
		Help: "have the models argue it out once, then settle it",
	},
}

// Names lists the verbs in the order they should be offered, cheapest first.
func Names() []string { return []string{"ask", "loop", "graph", "consensus", "debate"} }

// Lookup returns the strategy a verb name selects.
func Lookup(name string) (Strategy, bool) {
	s, ok := table[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))]
	return s, ok
}

// Split recognises a leading strategy verb on an input line and returns the
// strategy plus the remaining text.
//
// A bare verb with no task ("/loop") is NOT a strategy selection — it is a user
// asking what the verb does, and answering it with silence while quietly arming
// a mode would be the sticky-mode trap again. Callers show help instead.
func Split(line string) (Strategy, string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "/") {
		return Strategy{}, "", false
	}
	rest := s[1:]
	word := rest
	if i := strings.IndexAny(rest, " \t\n"); i >= 0 {
		word = rest[:i]
		rest = strings.TrimSpace(rest[i:])
	} else {
		rest = ""
	}
	st, ok := table[strings.ToLower(word)]
	if !ok || rest == "" {
		return Strategy{}, "", false
	}
	return st, rest, true
}

// Help renders the one-line description of every verb.
func Help() string {
	var b strings.Builder
	b.WriteString("Strategy verbs — each applies to that one message:\n")
	for _, n := range Names() {
		s := table[n]
		b.WriteString("  /" + pad(s.Name, 11) + s.Help + "\n")
	}
	b.WriteString("\n  /always <verb> keep using it until you say otherwise\n")
	b.WriteString("  /loop until `go test ./...` passes: <task>\n" +
		"                 stop when that command actually passes, not when the model says so\n")
	return b.String()
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
