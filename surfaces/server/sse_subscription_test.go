package server

import (
	"os"
	"regexp"
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
//
// The types are read out of core rather than restated here, so a constant
// added there cannot make this check pass by not looking at it.
var eventTypeDecl = regexp.MustCompile(`(?m)^\s*Event\w+\s+EventType\s*=\s*"([a-z_]+)"`)

func declaredEventTypes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../../infra/core/orchestrator_types.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range eventTypeDecl.FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	if len(out) < 15 {
		t.Fatalf("found only %d event types; the declaration moved and this check stopped checking", len(out))
	}
	return out
}

func TestEveryEventTypeHasABrowserSubscription(t *testing.T) {
	js, err := webFS.ReadFile("web/js/10-sse.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, et := range declaredEventTypes(t) {
		if !regexp.MustCompile(`"` + et + `"`).MatchString(src) {
			t.Errorf("the browser never subscribes to %q, so every one of those events is dropped", et)
		}
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
