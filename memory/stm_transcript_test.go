package memory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

func msg(role core.Role, text string) core.Message {
	return core.Message{Role: role, Content: text}
}

func newSys(t *testing.T) *System {
	t.Helper()
	s, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestCompactionDoesNotDestroyTheConversation is the regression.
//
// STMCompress replaced the window with a briefing plus a short tail and the
// turns it replaced were gone — from the model, from /log, and from any later
// question about what was said earlier. Compaction fires mid-task, so an agent
// that summarised its own working memory and then needed a detail out of it had
// no way back.
func TestCompactionDoesNotDestroyTheConversation(t *testing.T) {
	s := newSys(t)

	for i := 0; i < 12; i++ {
		s.STMAdd(msg(core.RoleUser, fmt.Sprintf("turn-%02d the secret is %d", i, i*7)))
	}

	s.STMCompress([]core.Message{msg(core.RoleSystem, "briefing: we discussed twelve things")}, 4)

	// The window shrank — that is the point of compacting.
	if got := len(s.STMGet()); got != 5 {
		t.Errorf("window holds %d messages, want briefing + 4 recent", got)
	}

	// ...and nothing was lost.
	full := renderAll(s.STMFullHistory())
	for i := 0; i < 12; i++ {
		want := fmt.Sprintf("turn-%02d", i)
		if !strings.Contains(full, want) {
			t.Errorf("%s is gone after compaction — the conversation was destroyed, not compacted", want)
		}
	}
	if !strings.Contains(full, "the secret is 21") {
		t.Error("a detail from a replaced turn is unrecoverable")
	}
}

// TestOverflowIsRetainedNotDropped — STMAdd trimmed at stmMax and discarded
// the front of the conversation with no record of it.
func TestOverflowIsRetainedNotDropped(t *testing.T) {
	s := newSys(t)

	const n = 120 // comfortably past the 50-message window
	for i := 0; i < n; i++ {
		s.STMAdd(msg(core.RoleUser, fmt.Sprintf("m%03d", i)))
	}

	if len(s.STMGet()) > 50 {
		t.Fatalf("window grew past its cap: %d", len(s.STMGet()))
	}
	if got := len(s.STMFullHistory()); got != n {
		t.Errorf("full history has %d of %d turns — the rest fell off the front", got, n)
	}
	if !strings.Contains(renderAll(s.STMFullHistory()), "m000") {
		t.Error("the first message of the session was discarded")
	}
}

// TestTranscriptDoesNotDoubleCountTheRetainedTail — the tail stays in the
// window, so recording it as well would report every kept turn twice.
func TestTranscriptDoesNotDoubleCountTheRetainedTail(t *testing.T) {
	s := newSys(t)
	for i := 0; i < 10; i++ {
		s.STMAdd(msg(core.RoleUser, fmt.Sprintf("t%d", i)))
	}
	s.STMCompress([]core.Message{msg(core.RoleSystem, "briefing")}, 3)

	counts := map[string]int{}
	for _, m := range s.STMFullHistory() {
		counts[m.ContentString()]++
	}
	for _, c := range []string{"t7", "t8", "t9"} {
		if counts[c] != 1 {
			t.Errorf("%s appears %d times in the full history, want 1", c, counts[c])
		}
	}
}

// TestNewSessionDropsTheTranscript — retaining it past a reset would let a
// fresh chat recall the previous one.
func TestNewSessionDropsTheTranscript(t *testing.T) {
	s := newSys(t)
	for i := 0; i < 80; i++ {
		s.STMAdd(msg(core.RoleUser, fmt.Sprintf("private-%d", i)))
	}
	if len(s.STMTranscript()) == 0 {
		t.Fatal("nothing was retained, so this test proves nothing")
	}

	s.STMClear()

	if got := len(s.STMTranscript()); got != 0 {
		t.Errorf("%d turns from the previous session survived a reset", got)
	}
	if got := len(s.STMFullHistory()); got != 0 {
		t.Errorf("full history is %d after a reset, want empty", got)
	}
}

// TestTranscriptIsBounded — a long-running session must not grow forever.
func TestTranscriptIsBounded(t *testing.T) {
	s := newSys(t)
	for i := 0; i < transcriptMax+s.stmMax+500; i++ {
		s.STMAdd(msg(core.RoleUser, fmt.Sprintf("x%d", i)))
	}
	if got := len(s.STMTranscript()); got > transcriptMax {
		t.Errorf("transcript grew to %d, past the %d cap", got, transcriptMax)
	}
	// The most recent retained turns are the ones kept.
	tr := s.STMTranscript()
	if len(tr) > 0 && tr[len(tr)-1].ContentString() == "x0" {
		t.Error("the cap dropped the newest turns instead of the oldest")
	}
}

// TestEmptyBriefingIsANoOp — the caller passes an empty briefing when
// compression failed, and that must not flush or shrink anything.
func TestEmptyBriefingIsANoOp(t *testing.T) {
	s := newSys(t)
	for i := 0; i < 10; i++ {
		s.STMAdd(msg(core.RoleUser, fmt.Sprintf("k%d", i)))
	}
	before := len(s.STMGet())

	s.STMCompress(nil, 4)

	if len(s.STMGet()) != before {
		t.Error("a failed compaction still shrank the window")
	}
	if len(s.STMTranscript()) != 0 {
		t.Error("a failed compaction flushed turns that never left the window")
	}
}

func renderAll(msgs []core.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.ContentString())
		b.WriteByte('\n')
	}
	return b.String()
}
