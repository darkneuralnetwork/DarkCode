package server

import (
	"regexp"
	"strings"
	"testing"
)

// An SSE frame is named (`event: <type>`), and EventSource only delivers a
// named event to a listener registered for that exact name. A type the browser
// never subscribes to is broadcast by the server and silently dropped — no
// error, no warning, the event simply never arrives.
//
// That is not hypothetical: file_change was emitted by every mutating tool
// call and reached no renderer, because the list in 10-sse.js was maintained
// by hand and had fallen behind core.EventType.

// TestEveryEventTypeHasABrowserSubscription used to grep 10-sse.js for a
// hardcoded array of quoted type names — the exact hand-maintained list that
// let file_change go unsubscribed (see the package comment above). 10-sse.js
// no longer hardcodes that list: it fetches it from /api/event-types
// (06-eventtypes.js), which serves core.EventTypes (infra/core/event_meta.go)
// — the same registry the CLI reads in-process. So the risk this test guards
// against moved: it's no longer "did someone forget to add a type to the JS
// list," it's "did someone reintroduce a second, hand-written list instead of
// reading the shared one." The Go side of "does the registry itself cover
// every declared type" is TestEventTypesCoversEveryConstant in
// infra/core/event_meta_test.go, which independently regex-extracts the
// declared constants from orchestrator_types.go's source rather than trusting
// core.EventTypes itself (so a missing registry entry can't hide by also
// being the thing doing the checking).
func TestEveryEventTypeHasABrowserSubscription(t *testing.T) {
	js, err := webFS.ReadFile("web/js/10-sse.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(src, "getEventTypes()") {
		t.Fatal("10-sse.js no longer reads its SSE listener types from the shared registry (getEventTypes) — a hand-written list here is exactly how file_change went unsubscribed before")
	}
}

// TestBrowserReadsTheTaskFieldByItsWireName. core.UIEvent marshals TaskID as
// `task_id`; the browser read `evt.task`, which is never present. The live
// plan board, Auto Mode's project activation and the per-task event grouping
// were all comparing against undefined.
func TestBrowserReadsTheTaskFieldByItsWireName(t *testing.T) {
	// Only reads off the event object. `dataset.task` is a DOM attribute the
	// feed owns and names itself, not a wire field.
	wrong := regexp.MustCompile(`\b(evt|data|event)\.task\b[^_]`)
	for _, path := range []string{"web/js/10-sse.js", "web/js/40-events.js", "web/js/220-v2.js"} {
		js, err := webFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if m := wrong.FindString(string(js)); m != "" {
			t.Errorf("%s reads %q; the wire field is task_id", path, m)
		}
	}
}
