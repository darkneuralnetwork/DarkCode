package cli

// verbs.go — the console's binding to the shared strategy verbs.
//
// The table itself lives in package verb so the web UI reads the same one; this
// file is only the console spelling of it. When the two surfaces each owned a
// copy, `/loop` worked in one and was sent to the model as literal text in the
// other.

import "github.com/darkcode/verb"

type strategy = verb.Strategy

// splitVerb recognises a leading strategy verb and returns the remaining text.
func splitVerb(line string) (strategy, string, bool) { return verb.Split(line) }

// StrategyNames lists the verbs, for help text and completion.
func StrategyNames() []string { return verb.Names() }

// verbHelp renders the one-line description of every verb.
func verbHelp() string { return verb.Help() }
