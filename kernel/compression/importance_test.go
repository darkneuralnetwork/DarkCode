package compression

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
)

// This is what decides which messages survive compression. Getting it wrong
// does not error — it silently drops the turn that mattered, and the model
// answers a question it can no longer see. The same family of mistake produced
// the "contents is not specified" 400.

func msg(role core.Role, text string) core.Message {
	return core.Message{Role: role, Content: text}
}

// TestScoreIsBounded. Every dimension is meant to be 0..1 and the weights sum
// to 1, so a total outside that range means a scorer is misbehaving and the
// threshold comparison stops meaning anything.
func TestScoreIsBounded(t *testing.T) {
	samples := []core.Message{
		msg(core.RoleUser, "why does /etc/hosts fail? error: permission denied"),
		msg(core.RoleAssistant, strings.Repeat("word ", 500)),
		msg(core.RoleSystem, ""),
		{Role: core.RoleAssistant, ToolCalls: make([]core.ToolCall, 20)},
		{Role: core.RoleTool, Content: "error: exit status 1"},
	}
	for i, m := range samples {
		s := ScoreMessage(m, i, len(samples), time.Now())
		for name, v := range map[string]float64{
			"ToolUsage": s.ToolUsage, "ErrorContent": s.ErrorContent,
			"UserIntent": s.UserIntent, "FileRefs": s.FileRefs,
			"Recency": s.Recency, "Total": s.Total,
		} {
			if v < 0 || v > 1 {
				t.Errorf("message %d: %s = %v, outside 0..1", i, name, v)
			}
		}
	}
}

// TestToolResultsOutrankPlainProse. A tool result carries what actually
// happened; dropping it while keeping the assistant's narration about it
// leaves the model reasoning from its own summary.
func TestToolResultsOutrankPlainProse(t *testing.T) {
	prose := ScoreMessage(msg(core.RoleAssistant, "I will look at that now."), 0, 10, time.Now())
	result := ScoreMessage(core.Message{Role: core.RoleTool, Content: "ok"}, 0, 10, time.Now())

	if result.Total <= prose.Total {
		t.Errorf("tool result scored %.3f, prose scored %.3f — a result must rank higher",
			result.Total, prose.Total)
	}
}

// TestMoreToolCallsScoreHigherButStayBounded. The score grows with the number
// of calls; without the cap a long batch would exceed 1 and distort the total.
func TestMoreToolCallsScoreHigherButStayBounded(t *testing.T) {
	one := ScoreMessage(core.Message{Role: core.RoleAssistant, ToolCalls: make([]core.ToolCall, 1)}, 0, 10, time.Now())
	three := ScoreMessage(core.Message{Role: core.RoleAssistant, ToolCalls: make([]core.ToolCall, 3)}, 0, 10, time.Now())
	many := ScoreMessage(core.Message{Role: core.RoleAssistant, ToolCalls: make([]core.ToolCall, 50)}, 0, 10, time.Now())

	if three.ToolUsage <= one.ToolUsage {
		t.Error("three tool calls did not outrank one")
	}
	if many.ToolUsage > 1.0 {
		t.Errorf("50 tool calls scored %v, above the cap", many.ToolUsage)
	}
}

// TestErrorsAreScoredAbovePlainText. An error is the context that explains why
// the next few turns look the way they do.
func TestErrorsAreScoredAbovePlainText(t *testing.T) {
	plain := ScoreMessage(msg(core.RoleAssistant, "the build finished"), 0, 10, time.Now())
	for _, text := range []string{
		"error: cannot find package",
		"the command failed with exit status 2",
		"panic: runtime error: index out of range",
	} {
		got := ScoreMessage(msg(core.RoleAssistant, text), 0, 10, time.Now())
		if got.ErrorContent <= plain.ErrorContent {
			t.Errorf("%q scored %.2f for error content, plain text scored %.2f",
				text, got.ErrorContent, plain.ErrorContent)
		}
	}
}

// TestRecencyFavoursTheEnd. The most recent turn is the one the next reply
// answers; if recency ran the other way compression would eat the live thread.
func TestRecencyFavoursTheEnd(t *testing.T) {
	const total = 20
	first := ScoreMessage(msg(core.RoleUser, "hello"), 0, total, time.Now())
	middle := ScoreMessage(msg(core.RoleUser, "hello"), total/2, total, time.Now())
	last := ScoreMessage(msg(core.RoleUser, "hello"), total-1, total, time.Now())

	if !(last.Recency > middle.Recency && middle.Recency > first.Recency) {
		t.Errorf("recency is not monotonic: first=%.3f middle=%.3f last=%.3f",
			first.Recency, middle.Recency, last.Recency)
	}
}

// TestRecencyHandlesDegenerateInput. A single-message conversation must not
// divide by zero or produce NaN — a NaN total compares false against every
// threshold, so the message would be silently unpinnable.
func TestRecencyHandlesDegenerateInput(t *testing.T) {
	for _, tc := range []struct{ idx, total int }{{0, 1}, {0, 0}, {5, 1}} {
		s := ScoreMessage(msg(core.RoleUser, "x"), tc.idx, tc.total, time.Now())
		if s.Total != s.Total { // NaN check
			t.Errorf("index %d of %d produced NaN", tc.idx, tc.total)
		}
		if s.Recency < 0 || s.Recency > 1 {
			t.Errorf("index %d of %d gave recency %v", tc.idx, tc.total, s.Recency)
		}
	}
}

// TestScoreMessagesAgreesWithIsPinned. Two entry points computing the same
// decision differently is how a message gets pinned by one path and dropped by
// the other.
func TestScoreMessagesAgreesWithIsPinned(t *testing.T) {
	messages := []core.Message{
		msg(core.RoleUser, "please refactor cmd/main.go and fix the error"),
		msg(core.RoleAssistant, "sure"),
		{Role: core.RoleTool, Content: "error: exit status 1"},
		msg(core.RoleAssistant, "done"),
	}

	scores, pinned := ScoreMessages(messages)
	if len(scores) != len(messages) {
		t.Fatalf("scored %d of %d messages", len(scores), len(messages))
	}

	inPinned := map[int]bool{}
	for _, i := range pinned {
		inPinned[i] = true
	}
	for i, m := range messages {
		if got, want := IsPinned(m, i, len(messages)), inPinned[i]; got != want {
			t.Errorf("message %d: IsPinned=%v but ScoreMessages pinned=%v (score %.3f)",
				i, got, want, scores[i].Total)
		}
	}
}

// TestFileReferencesAreNoticed. A message naming a path is how the model knows
// which file the conversation is about.
func TestFileReferencesAreNoticed(t *testing.T) {
	none := ScoreMessage(msg(core.RoleUser, "make it faster"), 0, 10, time.Now())
	withPath := ScoreMessage(msg(core.RoleUser, "make server/chat_handler.go faster"), 0, 10, time.Now())

	if withPath.FileRefs <= none.FileRefs {
		t.Errorf("a path reference scored %.2f, no reference scored %.2f",
			withPath.FileRefs, none.FileRefs)
	}
}

// TestEmptyConversationIsSafe. Compression runs on whatever it is handed.
func TestEmptyConversationIsSafe(t *testing.T) {
	scores, pinned := ScoreMessages(nil)
	if len(scores) != 0 || len(pinned) != 0 {
		t.Errorf("empty input produced %d scores and %d pins", len(scores), len(pinned))
	}
}

// TestColonFreeFailuresAreRecognised. Every indicator originally required a
// colon, so "exit status 1" — what a failed go build reports, and what the
// repair loop feeds back into the conversation — scored zero. The messages
// most worth keeping were the ones most likely to be dropped.
func TestColonFreeFailuresAreRecognised(t *testing.T) {
	plain := ScoreMessage(msg(core.RoleAssistant, "the build finished"), 0, 10, time.Now())

	for _, text := range []string{
		"the command failed with exit status 2",
		"go: build failed",
		"exit status 1",
		"--- FAIL: TestThing (0.00s)",
		"process exited with non-zero exit",
	} {
		got := ScoreMessage(msg(core.RoleTool, text), 0, 10, time.Now())
		if got.ErrorContent <= plain.ErrorContent {
			t.Errorf("%q scored %.2f for error content, ordinary text scored %.2f",
				text, got.ErrorContent, plain.ErrorContent)
		}
	}
}

// TestPassingSummaryIsNotScoredAsAFailure. Broadening the match must not turn
// every green test run into pinned context; a passing summary may register as
// weak, never as a hard failure.
func TestPassingSummaryIsNotScoredAsAFailure(t *testing.T) {
	hard := ScoreMessage(msg(core.RoleTool, "error: cannot find package"), 0, 10, time.Now())
	pass := ScoreMessage(msg(core.RoleTool, "ok  4 passed, 0 failed"), 0, 10, time.Now())

	if pass.ErrorContent >= hard.ErrorContent {
		t.Errorf("a passing summary scored %.2f, same or above a real failure at %.2f",
			pass.ErrorContent, hard.ErrorContent)
	}
}
