package server

import (
	"testing"

	"github.com/darkcode/verb"
)

// TestStripStrategyVerb — the web UI used to send "/loop fix the parser" to the
// model as literal text while the console understood it. The query the rest of
// the handler sees must be the task, not the instruction about how to run it.
func TestStripStrategyVerb(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantVerb  string
	}{
		{"/loop fix the parser", "fix the parser", "loop"},
		{"  /graph  add retries  ", "add retries", "graph"},
		{"/DEBATE which cache is right", "which cache is right", "debate"},
		{"fix the parser", "fix the parser", ""},           // no verb
		{"/loop", "/loop", ""},                             // bare verb: help, not a strategy
		{"/unknown do a thing", "/unknown do a thing", ""}, // not a strategy verb
		{"/ask about /loop", "about /loop", "ask"},         // only the leading verb counts
	}
	for _, tc := range cases {
		gotQuery, st, found := stripStrategyVerb(tc.in)
		if gotQuery != tc.wantQuery {
			t.Errorf("%q: query = %q, want %q", tc.in, gotQuery, tc.wantQuery)
		}
		if tc.wantVerb == "" {
			if found {
				t.Errorf("%q: found verb %q, want none", tc.in, st.Name)
			}
			continue
		}
		if !found || st.Name != tc.wantVerb {
			t.Errorf("%q: verb = %q (found=%v), want %q", tc.in, st.Name, found, tc.wantVerb)
		}
	}
}

// TestChatModeForVerbNeverBuildsOnAReadOnlyVerb. /ask promises not to change
// anything; routing it down a build path would break that promise regardless of
// what the tool scope does afterwards.
func TestChatModeForVerbNeverBuildsOnAReadOnlyVerb(t *testing.T) {
	ask, _ := verb.Lookup("ask")
	if got := chatModeForVerb(ask); got != "general" {
		t.Errorf("/ask chat mode = %q, want general (read-only)", got)
	}
	for _, name := range []string{"loop", "graph"} {
		st, _ := verb.Lookup(name)
		if got := chatModeForVerb(st); got != "loop" {
			t.Errorf("/%s chat mode = %q, want loop (it iterates)", name, got)
		}
	}
}

// TestEveryVerbMapsToAKnownChatMode. A verb that mapped to an unrecognised mode
// would silently fall through to whatever the default happens to be.
func TestEveryVerbMapsToAKnownChatMode(t *testing.T) {
	known := map[string]bool{"general": true, "project": true, "loop": true}
	for _, name := range verb.Names() {
		st, ok := verb.Lookup(name)
		if !ok {
			t.Fatalf("verb %q is listed but not defined", name)
		}
		if mode := chatModeForVerb(st); !known[mode] {
			t.Errorf("/%s maps to unknown chat mode %q", name, mode)
		}
	}
}
