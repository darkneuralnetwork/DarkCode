package verb

import (
	"fmt"
	"testing"
)

func TestSplitVerb(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantName string
		wantTask string
		wantOK   bool
	}{
		{"loop with task", "/loop add retry logic", "loop", "add retry logic", true},
		{"ask with task", "/ask how does the retry backoff work", "ask", "how does the retry backoff work", true},
		{"graph with task", "/graph migrate storage to postgres", "graph", "migrate storage to postgres", true},
		{"case insensitive", "/LOOP fix the build", "loop", "fix the build", true},
		{"carries an until clause", "/loop until `go test ./...` passes: add retries",
			"loop", "until `go test ./...` passes: add retries", true},

		// A bare verb is a request for help, not a mode switch. Arming a
		// strategy because someone typed a word and pressed enter is the
		// sticky-mode trap these verbs exist to avoid.
		{"bare verb", "/loop", "", "", false},
		{"bare verb with spaces", "/loop   ", "", "", false},

		// Consensus and debate were verbs and are not any more: how many
		// models answer is a session preference, so it lives in the routing
		// mode setting. A stale /consensus must fall through to the model as
		// ordinary text rather than silently doing nothing.
		{"consensus is no longer a verb", "/consensus is this migration safe", "", "", false},
		{"debate is no longer a verb", "/debate which cache is right", "", "", false},

		{"not a verb", "/help", "", "", false},
		{"not a command", "add retry logic", "", "", false},
		{"unknown verb", "/frobnicate the thing", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, task, ok := Split(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if st.Name != tc.wantName {
				t.Errorf("verb = %q, want %q", st.Name, tc.wantName)
			}
			if task != tc.wantTask {
				t.Errorf("task = %q, want %q", task, tc.wantTask)
			}
		})
	}
}

// TestVerbsSelectDistinctStrategies. A verb is only worth having if it changes
// something; two verbs that resolve to the same overrides are one verb with two
// spellings.
func TestVerbsSelectDistinctStrategies(t *testing.T) {
	seen := map[string]string{}
	for _, n := range Names() {
		s := table[n]
		key := fmt.Sprintf("%s|%s|%s|%s", s.Loop, s.Tools, s.Mode, s.Plan)
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q select identical strategies (%s)", n, prev, key)
		}
		seen[key] = n
	}
}

// TestAskIsReadOnly is the one that would be a real problem to get wrong: /ask
// must never be able to change anything.
func TestAskIsReadOnly(t *testing.T) {
	if table["ask"].Tools != "readonly" {
		t.Errorf("/ask tools = %q, want readonly", table["ask"].Tools)
	}
	if table["ask"].Loop != "off" {
		t.Errorf("/ask should not iterate, got loop=%q", table["ask"].Loop)
	}
}

// TestNoVerbChangesRoutingMode. Every verb here is a one-shot override on how
// a single message is worked; how many models answer is a standing preference
// and lives in routing_mode. When both surfaces could say it, the sticky one
// won silently whenever they disagreed — a verb that quietly loses to a setting
// is worse than no verb, and that is the trap this package exists to remove.
func TestNoVerbChangesRoutingMode(t *testing.T) {
	for name, s := range table {
		if s.Mode != "" {
			t.Errorf("/%s sets routing mode %q; that belongs in configuration, "+
				"where it applies to every query rather than one", name, s.Mode)
		}
	}
}

// TestGraphPlansAndLoopDoesNot is the difference between the two verbs. Both
// iterate; /graph always decomposes first so there are per-task acceptance
// criteria to prove. Without that they select identical behaviour and /graph
// is a synonym rather than a strategy.
func TestGraphPlansAndLoopDoesNot(t *testing.T) {
	if table["graph"].Plan != "always" {
		t.Errorf("/graph plan = %q, want always", table["graph"].Plan)
	}
	if table["loop"].Plan == "always" {
		t.Error("/loop should not force a planning phase; that is what /graph is for")
	}
}

// TestLoopAndGraphEnableTheLoop — both are execution strategies, and neither
// depends on a persistent flag any more. The old coupling meant picking Loop
// could silently do nothing.
func TestLoopAndGraphEnableTheLoop(t *testing.T) {
	for _, n := range []string{"loop", "graph"} {
		if table[n].Loop != "on" {
			t.Errorf("/%s loop = %q, want on", n, table[n].Loop)
		}
		if table[n].Tools != "on" {
			t.Errorf("/%s tools = %q, want on", n, table[n].Tools)
		}
	}
}
