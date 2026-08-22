package server

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/router"
	"github.com/darkcode/ui"
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
		{"/ASK which cache is right", "which cache is right", "ask"}, // case-insensitive
		{"/consensus is this safe", "/consensus is this safe", ""},   // retired: routing mode owns fan-out
		{"fix the parser", "fix the parser", ""},                     // no verb
		{"/loop", "/loop", ""},                                       // bare verb: help, not a strategy
		{"/unknown do a thing", "/unknown do a thing", ""},           // not a strategy verb
		{"/ask about /loop", "about /loop", "ask"},                   // only the leading verb counts
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

// TestChatModeForEffort maps every rung on the escalation ladder onto a chat
// mode the handler understands. An unmapped rung would fall through to whatever
// the default happens to be, silently.
func TestChatModeForEffort(t *testing.T) {
	want := map[router.Effort]string{
		router.EffortAsk:       "general", // read-only
		router.EffortDirect:    "project",
		router.EffortLoop:      "loop",
		router.EffortGraph:     "loop",
		router.EffortConsensus: "project",
	}
	for e, w := range want {
		if got := chatModeForEffort(e); got != w {
			t.Errorf("chatModeForEffort(%q) = %q, want %q", e, got, w)
		}
	}
}

// TestRoutingCostsNoModelCall. The classifier this replaced spent a call with a
// 12-second timeout before any work started, and was the first thing a metered
// tier rate-limited. Routing must now be reachable with no provider at all.
func TestRoutingCostsNoModelCall(t *testing.T) {
	for _, q := range []string{
		"what does the cascade do?",
		"build a website",
		"add a retry to the http client",
	} {
		e, why := router.EntryEffort(q) // no client, no context, no network
		if e == "" || why == "" {
			t.Errorf("EntryEffort(%q) = %q/%q — routing must be total", q, e, why)
		}
	}
}

// TestAnnounceStaysQuietForTheDefault. An announcement on every ordinary
// message would be the most frequent event in the feed and the first one people
// learn to skip — and there is no verb to teach for the rung you get by typing
// nothing.
func TestAnnounceStaysQuietForTheDefault(t *testing.T) {
	if router.EffortDirect.Verb() != "" {
		t.Fatal("the default rung gained a verb; the quiet rule below needs revisiting")
	}
	var seen []string
	em := ui.NewEventEmitter(true, io.Discard)
	em.OnHandler(func(ev core.UIEvent) {
		if ev.TaskID == "strategy" {
			seen = append(seen, contentString(ev.Content))
		}
	})
	s := &Server{emitter: em}

	s.announceEffort(router.EffortDirect, "a single pass with tools should cover it")
	if len(seen) != 0 {
		t.Errorf("announced the default rung: %v", seen)
	}

	s.announceEffort(router.EffortLoop, "the request is multi-step")
	if len(seen) != 1 {
		t.Fatalf("a non-default rung produced %d announcements, want 1", len(seen))
	}
	if !strings.Contains(seen[0], "/loop") {
		t.Errorf("announcement does not name the verb: %q", seen[0])
	}
	if !strings.Contains(seen[0], "multi-step") {
		t.Errorf("announcement does not say why: %q", seen[0])
	}
}

// contentString renders a UIEvent's content field, whatever concrete type the
// emitter put in it.
func contentString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
