package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// QA audit Finding 4: /v1/chat/completions silently discarded every message
// but the last user turn. buildCompatPrompt is the extracted, pure
// request-shaping logic — these tests don't need a live model or a wired
// kernel, since folding prior turns into the prompt is deterministic string
// construction.

func rawStr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestBuildCompatPromptSingleTurnUnaffected(t *testing.T) {
	prompt, ok := buildCompatPrompt([]compatMessage{
		{Role: "user", Content: rawStr("what is the module name?")},
	})
	if !ok {
		t.Fatal("expected ok=true for a single user message")
	}
	if prompt != "what is the module name?" {
		t.Fatalf("single-turn prompt should be unchanged, got %q", prompt)
	}
}

func TestBuildCompatPromptFoldsEarlierTurnsInsteadOfDroppingThem(t *testing.T) {
	prompt, ok := buildCompatPrompt([]compatMessage{
		{Role: "system", Content: rawStr("You are a helpful assistant.")},
		{Role: "user", Content: rawStr("My module is called qaworkspace.")},
		{Role: "assistant", Content: rawStr("Got it.")},
		{Role: "user", Content: rawStr("What is the module name?")},
	})
	if !ok {
		t.Fatal("expected ok=true")
	}
	for _, want := range []string{"You are a helpful assistant.", "qaworkspace", "Got it.", "What is the module name?"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected folded prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	// The newest user turn must still be what the answer is actually about —
	// context is prepended, not appended after it.
	if !strings.HasSuffix(strings.TrimSpace(prompt), "What is the module name?") {
		t.Fatalf("expected the latest user turn last in the prompt, got:\n%s", prompt)
	}
}

func TestBuildCompatPromptNoUserMessage(t *testing.T) {
	_, ok := buildCompatPrompt([]compatMessage{
		{Role: "system", Content: rawStr("You are a helpful assistant.")},
	})
	if ok {
		t.Fatal("expected ok=false when there is no user message")
	}
}

func TestBuildCompatPromptAcceptsTypedContentParts(t *testing.T) {
	parts, _ := json.Marshal([]map[string]string{{"type": "text", "text": "hello "}, {"type": "text", "text": "world"}})
	prompt, ok := buildCompatPrompt([]compatMessage{
		{Role: "user", Content: parts},
	})
	if !ok || prompt != "hello world" {
		t.Fatalf("expected typed content parts to be concatenated, got ok=%v prompt=%q", ok, prompt)
	}
}
