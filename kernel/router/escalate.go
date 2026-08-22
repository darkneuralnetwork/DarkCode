package router

// escalate.go — picking a strategy from signals instead of from a prediction.
//
// The standard 2026 approach routes with a classifier: one model call, before
// any work happens, guessing how hard the request will turn out to be. That
// call is pure overhead on a metered tier and it is the first thing to 429 —
// and when it failed here the request silently fell back to a keyword scan
// anyway, which is the tell that the call was never carrying its weight.
//
// Progressive complexity escalation inverts it: start at the cheapest effort that
// could plausibly work and climb only when something that actually happened
// says it was not enough. The signals are all by-products of doing the work —
// a repeated failing call, an acceptance check that will not pass — so routing
// costs nothing and reacts to evidence rather than to a guess about it.
//
// The ladder runs in both directions on purpose. Escalation alone ratchets: a
// task that climbed once would stay expensive for the rest of the run.

import "strings"

// Effort is a point on the strategy ladder, cheapest first. The names match the
// strategy verbs wherever there is one, so an announcement teaches the verb
// that would have selected the same thing.
type Effort string

const (
	EffortAsk       Effort = "ask"       // read-only, one call
	EffortDirect    Effort = "direct"    // tools, one turn — the unnamed default
	EffortLoop      Effort = "loop"      // iterate until the checks pass
	EffortGraph     Effort = "graph"     // decompose, run waves, prove each node
	EffortConsensus Effort = "consensus" // bring the other models in
)

// ladder is the ordering. Everything below is expressed as movement along it,
// so no signal can invent an effort or skip the sequence by accident.
var ladder = []Effort{EffortAsk, EffortDirect, EffortLoop, EffortGraph, EffortConsensus}

// effortIndex reports an effort's position, or -1 for an unknown one.
func effortIndex(e Effort) int {
	for i, x := range ladder {
		if x == e {
			return i
		}
	}
	return -1
}

// Verb returns the strategy verb that selects this effort, or "" for the one
// that has no verb. Announcements use it to name the shortcut.
func (e Effort) Verb() string {
	if e == EffortDirect {
		return "" // "use tools once" is the default; there is nothing to type
	}
	return string(e)
}

// entryLoopComplexity is the AssessComplexity score at which a fresh request
// starts by iterating rather than answering in one turn. The scale's baseline
// is 3 and a single incidental keyword reaches 4, so the bar sits above the
// range a one-line request can hit by accident.
const entryLoopComplexity = 6

// EntryEffort picks the cheapest effort that could plausibly work, and says why.
//
// This runs before any model call. Being wrong here is cheap by design: the
// signals below will move it, and an effort that turned out too low costs one
// escalation, where starting everything at the top costs every request.
func EntryEffort(query string) (Effort, string) {
	if strings.TrimSpace(query) == "" {
		return EffortDirect, "empty request"
	}
	if IsGeneralQuestion(query) {
		return EffortAsk, "reads as a question, so nothing needs changing"
	}
	if LooksLikeBuild(query) {
		return EffortLoop, "asks for something to be built, which takes more than one turn"
	}
	if c := AssessComplexity(query); c >= entryLoopComplexity {
		return EffortLoop, "the request is multi-step"
	}
	return EffortDirect, "a single pass with tools should cover it"
}

// Signal is something that happened during the run which the current effort did
// not handle.
type Signal int

const (
	// SignalNeedsTools: a read-only turn turned out to need to change something.
	SignalNeedsTools Signal = iota
	// SignalStuck: the same call keeps failing. Retrying it again is the one
	// response guaranteed not to work, so decompose instead.
	SignalStuck
	// SignalUnproven: the acceptance checks still fail after the repair rounds.
	SignalUnproven
	// SignalPlanIsSingleNode: the decomposition produced one task, so the graph
	// machinery is overhead with nothing to schedule.
	SignalPlanIsSingleNode
)

// Next returns the effort to move to, with the reason to announce. ok is false
// when the signal does not move it — already at the end of the ladder, or the
// signal does not apply to the current effort.
//
// A silent strategy change is indistinguishable from a bug when the cost or
// latency jumps, so the reason is not optional.
func Next(from Effort, s Signal) (Effort, string, bool) {
	i := effortIndex(from)
	if i < 0 {
		return from, "", false
	}

	switch s {
	case SignalNeedsTools:
		if from != EffortAsk {
			return from, "", false
		}
		return EffortDirect, "this needs to change files, not just read them", true

	case SignalStuck:
		// Straight to graph from anywhere below it: the problem is that the
		// work has not been broken up, and another iteration of the same shape
		// is what is already failing.
		if i >= effortIndex(EffortGraph) {
			return from, "", false
		}
		return EffortGraph, "the same step keeps failing, so the work needs breaking up", true

	case SignalUnproven:
		if i >= len(ladder)-1 {
			return from, "", false
		}
		return ladder[i+1], "the checks still aren't passing", true

	case SignalPlanIsSingleNode:
		// The only downward move. Without it the ladder ratchets and a run that
		// escalated once stays expensive to the end.
		if from != EffortGraph {
			return from, "", false
		}
		return EffortLoop, "the plan came back as one task, so there is nothing to schedule", true
	}
	return from, "", false
}

// LooksLikeBuild reports whether a query asks for something to be built: a
// creation verb plus an artifact noun.
//
// It lives here rather than in the HTTP handler because the console needs the
// same answer, and because it is now a routing signal in its own right instead
// of a fallback for when the classifier was unavailable.
func LooksLikeBuild(query string) bool {
	q := strings.ToLower(query)
	hasVerb := false
	for _, v := range BuildVerbs {
		if strings.Contains(q, v+" ") {
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		return false
	}
	for _, noun := range buildNouns {
		if strings.Contains(q, noun) {
			return true
		}
	}
	return false
}

// BuildVerbs are the creation verbs that mark a query as a build task.
var BuildVerbs = []string{
	"create", "build", "make", "implement", "develop", "write", "generate",
	"scaffold", "set up", "setup", "bootstrap", "design and",
}

var buildNouns = []string{
	"app", "website", "web page", "webpage", "site", "api", "service", "server",
	"script", "program", "tool", "page", "dashboard", "bot", "game", "cli",
}

// IsBuildShaped reports whether a query should get a project: anything that
// will iterate, plus a single-pass request that leads with a creation verb.
// Questions and read-only tasks stay project-less.
func IsBuildShaped(query string, effort Effort) bool {
	if effort == EffortAsk {
		return false
	}
	if effortIndex(effort) >= effortIndex(EffortLoop) {
		return true
	}
	q := strings.ToLower(strings.TrimSpace(query))
	head := q
	if len(head) > 80 {
		head = head[:80]
	}
	for _, v := range BuildVerbs {
		if strings.HasPrefix(q, v+" ") || strings.Contains(head, " "+v+" ") {
			return true
		}
	}
	return false
}
