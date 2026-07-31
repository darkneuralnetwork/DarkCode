package cli

import "testing"

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
		{"consensus with task", "/consensus is this migration safe", "consensus", "is this migration safe", true},
		{"case insensitive", "/LOOP fix the build", "loop", "fix the build", true},
		{"carries an until clause", "/loop until `go test ./...` passes: add retries",
			"loop", "until `go test ./...` passes: add retries", true},

		// A bare verb is a request for help, not a mode switch. Arming a
		// strategy because someone typed a word and pressed enter is the
		// sticky-mode trap these verbs exist to avoid.
		{"bare verb", "/loop", "", "", false},
		{"bare verb with spaces", "/loop   ", "", "", false},

		{"not a verb", "/help", "", "", false},
		{"not a command", "add retry logic", "", "", false},
		{"unknown verb", "/frobnicate the thing", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, task, ok := splitVerb(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if st.name != tc.wantName {
				t.Errorf("verb = %q, want %q", st.name, tc.wantName)
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
	for _, n := range StrategyNames() {
		s := strategies[n]
		key := s.loop + "|" + s.tools + "|" + s.mode + "|" + s.plan
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q select identical strategies (%s)", n, prev, key)
		}
		seen[key] = n
	}
}

// TestAskIsReadOnly is the one that would be a real problem to get wrong: /ask
// must never be able to change anything.
func TestAskIsReadOnly(t *testing.T) {
	if strategies["ask"].tools != "readonly" {
		t.Errorf("/ask tools = %q, want readonly", strategies["ask"].tools)
	}
	if strategies["ask"].loop != "off" {
		t.Errorf("/ask should not iterate, got loop=%q", strategies["ask"].loop)
	}
}

// TestGraphPlansAndLoopDoesNot is the difference between the two verbs. Both
// iterate; /graph always decomposes first so there are per-task acceptance
// criteria to prove. Without that they select identical behaviour and /graph
// is a synonym rather than a strategy.
func TestGraphPlansAndLoopDoesNot(t *testing.T) {
	if strategies["graph"].plan != "always" {
		t.Errorf("/graph plan = %q, want always", strategies["graph"].plan)
	}
	if strategies["loop"].plan == "always" {
		t.Error("/loop should not force a planning phase; that is what /graph is for")
	}
}

// TestLoopAndGraphEnableTheLoop — both are execution strategies, and neither
// depends on a persistent flag any more. The old coupling meant picking Loop
// could silently do nothing.
func TestLoopAndGraphEnableTheLoop(t *testing.T) {
	for _, n := range []string{"loop", "graph"} {
		if strategies[n].loop != "on" {
			t.Errorf("/%s loop = %q, want on", n, strategies[n].loop)
		}
		if strategies[n].tools != "on" {
			t.Errorf("/%s tools = %q, want on", n, strategies[n].tools)
		}
	}
}
