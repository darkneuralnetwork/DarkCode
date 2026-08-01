package router

import "testing"

// TestEntryEffortIsDeterministic is the whole point: routing happens before any
// model call, so a rate-limited provider cannot strand the request the way the
// classifier's 12-second timeout could.
func TestEntryEffortIsDeterministic(t *testing.T) {
	const q = "refactor the parser and add tests"
	first, reason := EntryEffort(q)
	for i := 0; i < 50; i++ {
		if got, _ := EntryEffort(q); got != first {
			t.Fatalf("EntryEffort is not deterministic: %q then %q", first, got)
		}
	}
	if reason == "" {
		t.Error("an effort with no reason cannot be announced")
	}
}

func TestEntryEffort(t *testing.T) {
	cases := []struct {
		query string
		want  Effort
	}{
		// Questions read nothing and change nothing.
		{"what does the cascade do?", EffortAsk},
		{"why is the cache replaying old errors?", EffortAsk},
		{"how do I enable the sandbox?", EffortAsk},

		// Build requests take more than one turn.
		{"build a website for tracking runs", EffortLoop},
		{"create a cli tool that lints the config", EffortLoop},

		// Multi-step work, by score.
		{"refactor the parser, debug the failing migration, and optimize the query path", EffortLoop},

		// Everything else starts cheap.
		{"add a retry to the http client", EffortDirect},
		{"rename Gate to Guard in permission/gate.go", EffortDirect},
		{"", EffortDirect},
	}
	for _, tc := range cases {
		if got, _ := EntryEffort(tc.query); got != tc.want {
			t.Errorf("EntryEffort(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// TestEntryEffortStartsBelowTheTop. Starting anything at graph or consensus
// unprompted is how a metered tier gets drained; those must be reached by
// evidence, never guessed into.
func TestEntryEffortStartsBelowTheTop(t *testing.T) {
	queries := []string{
		"build a distributed database with concurrent migration and security architecture",
		"architect and design a multi-service deployment with parallel integration tests",
		"what is this?",
		"fix it",
	}
	for _, q := range queries {
		got, _ := EntryEffort(q)
		if effortIndex(got) > effortIndex(EffortLoop) {
			t.Errorf("EntryEffort(%q) = %q — entry must never exceed loop", q, got)
		}
	}
}

// TestStuckGoesToGraph. Another iteration of the same shape is what is already
// failing, so the escalation has to change the shape.
func TestStuckGoesToGraph(t *testing.T) {
	for _, from := range []Effort{EffortAsk, EffortDirect, EffortLoop} {
		got, reason, ok := Next(from, SignalStuck)
		if !ok {
			t.Errorf("stuck at %q did not escalate", from)
			continue
		}
		if got != EffortGraph {
			t.Errorf("stuck at %q → %q, want graph", from, got)
		}
		if reason == "" {
			t.Errorf("stuck at %q escalated silently", from)
		}
	}
	// Already decomposed: there is nowhere better for this signal to go.
	if _, _, ok := Next(EffortGraph, SignalStuck); ok {
		t.Error("stuck at graph escalated again; decomposing twice is not a fix")
	}
}

// TestUnprovenClimbsOneRung — not straight to the top.
func TestUnprovenClimbsOneRung(t *testing.T) {
	steps := []struct{ from, want Effort }{
		{EffortAsk, EffortDirect},
		{EffortDirect, EffortLoop},
		{EffortLoop, EffortGraph},
		{EffortGraph, EffortConsensus},
	}
	for _, s := range steps {
		got, reason, ok := Next(s.from, SignalUnproven)
		if !ok || got != s.want {
			t.Errorf("unproven at %q → %q (ok=%v), want %q", s.from, got, ok, s.want)
		}
		if ok && reason == "" {
			t.Errorf("unproven at %q escalated silently", s.from)
		}
	}
}

// TestTheLadderTerminates. Consensus is the top; without this an unproven run
// could climb forever.
func TestTheLadderTerminates(t *testing.T) {
	at := EffortAsk
	for i := 0; i < len(ladder)*3; i++ {
		next, _, ok := Next(at, SignalUnproven)
		if !ok {
			if at != EffortConsensus {
				t.Fatalf("ladder stopped at %q, want consensus", at)
			}
			return
		}
		at = next
	}
	t.Fatal("the ladder never terminated")
}

// TestDeEscalationBreaksTheRatchet. Escalation alone means a run that climbed
// once stays expensive to the end.
func TestDeEscalationBreaksTheRatchet(t *testing.T) {
	got, reason, ok := Next(EffortGraph, SignalPlanIsSingleNode)
	if !ok {
		t.Fatal("a one-node plan did not de-escalate")
	}
	if got != EffortLoop {
		t.Errorf("one-node plan → %q, want loop", got)
	}
	if effortIndex(got) >= effortIndex(EffortGraph) {
		t.Error("de-escalation did not actually go down")
	}
	if reason == "" {
		t.Error("de-escalation must be announced too")
	}
	// It only applies to graph: a one-node plan is not a signal anywhere else.
	for _, from := range []Effort{EffortAsk, EffortDirect, EffortLoop, EffortConsensus} {
		if _, _, ok := Next(from, SignalPlanIsSingleNode); ok {
			t.Errorf("a one-node plan moved %q, which never planned", from)
		}
	}
}

// TestNeedsToolsOnlyLiftsAsk. Every other rung already has tools, so the signal
// must not nudge them anywhere.
func TestNeedsToolsOnlyLiftsAsk(t *testing.T) {
	got, _, ok := Next(EffortAsk, SignalNeedsTools)
	if !ok || got != EffortDirect {
		t.Errorf("ask + needs-tools → %q (ok=%v), want direct", got, ok)
	}
	for _, from := range []Effort{EffortDirect, EffortLoop, EffortGraph, EffortConsensus} {
		if _, _, ok := Next(from, SignalNeedsTools); ok {
			t.Errorf("needs-tools moved %q, which already has them", from)
		}
	}
}

// TestUnknownEffortNeverMoves guards the boundary: a typo'd effort must not
// silently route to the cheapest or the most expensive strategy.
func TestUnknownEffortNeverMoves(t *testing.T) {
	for _, s := range []Signal{SignalNeedsTools, SignalStuck, SignalUnproven, SignalPlanIsSingleNode} {
		if got, _, ok := Next(Effort("nonsense"), s); ok {
			t.Errorf("an unknown effort routed to %q", got)
		}
	}
}

// TestEffortVerbNamesTheShortcut. Announcements teach the verbs; an effort whose
// name is not a verb would advertise something the user cannot type.
func TestEffortVerbNamesTheShortcut(t *testing.T) {
	if EffortDirect.Verb() != "" {
		t.Error("direct has no verb — it is what happens when you type nothing")
	}
	for _, r := range []Effort{EffortAsk, EffortLoop, EffortGraph, EffortConsensus} {
		if r.Verb() != string(r) {
			t.Errorf("effort %q advertises verb %q", r, r.Verb())
		}
	}
}

func TestIsBuildShaped(t *testing.T) {
	// Anything that iterates gets a project.
	for _, r := range []Effort{EffortLoop, EffortGraph, EffortConsensus} {
		if !IsBuildShaped("tidy up the imports", r) {
			t.Errorf("effort %q should be build-shaped", r)
		}
	}
	// Questions never do, however they are phrased.
	if IsBuildShaped("create a new project?", EffortAsk) {
		t.Error("a question got a project")
	}
	// A single pass leading with a creation verb does.
	if !IsBuildShaped("write a parser for the log format", EffortDirect) {
		t.Error("a creation verb in a single pass should be build-shaped")
	}
	if IsBuildShaped("rename Gate to Guard", EffortDirect) {
		t.Error("a rename is not a build")
	}
}

// The cases below moved with LooksLikeBuild when it stopped being the
// classifier's fallback and became the routing signal itself.

func TestLooksLikeBuild(t *testing.T) {
	build := []string{
		"build a simple todo list web page with add and delete",
		"create a python flask website for blog posting",
		"make me a CLI tool for renaming files",
		"write a script that scrapes a site",
	}
	for _, q := range build {
		if !LooksLikeBuild(q) {
			t.Errorf("LooksLikeBuild(%q) = false, want true", q)
		}
	}
	notBuild := []string{
		"what is the capital of France",
		"explain how flask handles routing",
		"summarize this file",
		"build up my confidence", // verb but no artifact noun
	}
	for _, q := range notBuild {
		if LooksLikeBuild(q) {
			t.Errorf("LooksLikeBuild(%q) = true, want false", q)
		}
	}
}

func TestIsBuildShapedCases(t *testing.T) {
	cases := []struct {
		query  string
		effort Effort
		want   bool
	}{
		{"create a python flask website for blog posting", EffortDirect, true},
		{"anything at all", EffortLoop, true},
		{"please build me a REST API", EffortDirect, true},
		{"set up a new react app", EffortDirect, true},
		{"what does the auth middleware do?", EffortDirect, false},
		{"explain how JWT works", EffortAsk, false},
		{"read the config file", EffortDirect, false},
	}
	for _, c := range cases {
		if got := IsBuildShaped(c.query, c.effort); got != c.want {
			t.Errorf("IsBuildShaped(%q, %s) = %v, want %v", c.query, c.effort, got, c.want)
		}
	}
}
