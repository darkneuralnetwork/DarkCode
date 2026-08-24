package orchestrator

import (
	"testing"

	"github.com/darkcode/infra/core"
)

func msg(role core.Role, content string) core.Message {
	return core.Message{Role: role, Content: content}
}

func TestBoundedChatContext(t *testing.T) {
	// Short conversation → unchanged.
	short := []core.Message{msg(core.RoleUser, "hi"), msg(core.RoleAssistant, "hello")}
	if got := boundedChatContext(short); len(got) != 2 {
		t.Fatalf("short convo should pass through, got %d", len(got))
	}

	// Long conversation: a rolling summary + many turns. Expect the summary kept
	// plus only the last chatContextRecentMax messages.
	var long []core.Message
	long = append(long, msg(core.RoleSystem, "[COMPRESSED CONTEXT]\ngoal: build X\n[/COMPRESSED CONTEXT]"))
	for i := 0; i < 20; i++ {
		long = append(long, msg(core.RoleUser, "q"), msg(core.RoleAssistant, "a"))
	}
	got := boundedChatContext(long)
	// summary (1) + last chatContextRecentMax verbatim.
	if len(got) != 1+chatContextRecentMax {
		t.Fatalf("expected summary + %d recent, got %d", chatContextRecentMax, len(got))
	}
	if s, _ := got[0].Content.(string); got[0].Role != core.RoleSystem || !contains(s, "[COMPRESSED CONTEXT]") {
		t.Errorf("first message should be the rolling summary, got %+v", got[0])
	}
	if len(got) >= len(long) {
		t.Errorf("bounded context (%d) should be much smaller than the full transcript (%d)", len(got), len(long))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
