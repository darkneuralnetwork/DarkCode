package core

import (
	"os"
	"regexp"
	"testing"
)

// eventTypeDecl extracts declared EventType constant values directly from
// orchestrator_types.go's source, rather than trusting a hand-maintained
// Go-level list — the same independent-extraction approach
// surfaces/server/sse_subscription_test.go uses for the same reason: a list
// that's also the thing being checked can't catch its own omissions.
var eventTypeDecl = regexp.MustCompile(`(?m)^\s*Event\w+\s+EventType\s*=\s*"([a-z_]+)"`)

func declaredEventTypeConstants(t *testing.T) []EventType {
	t.Helper()
	src, err := os.ReadFile("orchestrator_types.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []EventType
	for _, m := range eventTypeDecl.FindAllStringSubmatch(string(src), -1) {
		out = append(out, EventType(m[1]))
	}
	if len(out) < 15 {
		t.Fatalf("found only %d event types; the declaration moved and this check stopped checking", len(out))
	}
	return out
}

func TestEventTypesHasNoDuplicateKeys(t *testing.T) {
	seen := make(map[EventType]bool, len(EventTypes))
	for _, em := range EventTypes {
		if seen[em.Type] {
			t.Errorf("duplicate EventType %q in EventTypes", em.Type)
		}
		seen[em.Type] = true
	}
}

// TestEventTypesCoversEveryConstant fails loudly if a new core.EventType
// constant is ever added without a matching entry in EventTypes — the
// guardrail that makes the registry load-bearing rather than just relocated
// code that can silently go stale again.
func TestEventTypesCoversEveryConstant(t *testing.T) {
	declared := declaredEventTypeConstants(t)
	for _, et := range declared {
		got := Lookup(et)
		if got == defaultEventMeta {
			t.Errorf("EventType %q has no registry entry (Lookup fell back to the default) — add it to EventTypes in event_meta.go", et)
		}
		if got.Type != et {
			t.Errorf("Lookup(%q).Type = %q, want %q", et, got.Type, et)
		}
	}
	if len(EventTypes) != len(declared) {
		t.Errorf("EventTypes has %d entries but there are %d declared EventType constants — one list is stale", len(EventTypes), len(declared))
	}
}

func TestLookupUnknownTypeReturnsDefault(t *testing.T) {
	got := Lookup(EventType("not_a_real_type"))
	if got != defaultEventMeta {
		t.Errorf("Lookup of an unknown type = %+v, want the default %+v", got, defaultEventMeta)
	}
}
