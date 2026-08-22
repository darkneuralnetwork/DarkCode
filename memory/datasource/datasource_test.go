package datasource

import (
	"testing"
	"time"

	"github.com/darkcode/memory/memory"
)

func newStore(t *testing.T) *memory.System {
	t.Helper()
	s, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestRetrieveDropsPriorConversationButKeepsFacts is the epoch filter.
//
// A new chat must not resurface the last one, but durable knowledge is not
// conversation and has to survive the boundary. Both kinds are stored before
// the epoch here, so only the classification separates them.
func TestRetrieveDropsPriorConversationButKeepsFacts(t *testing.T) {
	s := newStore(t)
	// "task:"-keyed entries are the per-Q&A facts written for a chat turn;
	// anything else semantic is durable.
	if err := s.SemanticAdd("task:parser question", "the parser builds a tree", "note", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SemanticAdd("parser-design", "the parser builds a tree", "note", nil); err != nil {
		t.Fatal(err)
	}

	m := New(s)
	epoch := time.Now().Add(time.Hour) // everything stored above is "before" it

	got := m.Retrieve(Query{Goal: "parser tree", K: 10, SinceEpoch: epoch})
	for _, h := range got.Hits {
		if h.ID == "task:parser question" {
			t.Error("a prior conversation turn survived the session epoch")
		}
	}
	var keptDurable bool
	for _, h := range got.Hits {
		if h.ID == "parser-design" {
			keptDurable = true
		}
	}
	if !keptDurable {
		t.Error("a durable fact was dropped by the epoch filter; only conversation is session-scoped")
	}
}

// TestRetrieveHonoursK — the filter runs against a wider set and trims after,
// so dropping stale conversation cannot starve out facts ranked below it.
func TestRetrieveHonoursK(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 12; i++ {
		if err := s.SemanticAdd(string(rune('a'+i))+"-parser", "the parser builds a tree", "note", nil); err != nil {
			t.Fatal(err)
		}
	}
	m := New(s)
	if got := len(m.Retrieve(Query{Goal: "parser tree", K: 3}).Hits); got > 3 {
		t.Errorf("Retrieve returned %d hits for K=3", got)
	}
	if got := len(m.Retrieve(Query{Goal: "parser tree", K: 0}).Hits); got != 0 {
		t.Errorf("K=0 returned %d hits", got)
	}
}

// TestGatewayIsNilSafe — the kernel holds this manager unconditionally, so
// every read has to degrade rather than panic when there is no store behind it.
func TestGatewayIsNilSafe(t *testing.T) {
	m := New(nil)
	if got := m.Retrieve(Query{Goal: "x", K: 5}); len(got.Hits) != 0 {
		t.Error("Retrieve returned hits with no store")
	}
	if _, ok := m.ConfidentRecall("x", 0); ok {
		t.Error("ConfidentRecall answered with no store")
	}
	if _, ok := m.AnswerFromGraph("x"); ok {
		t.Error("AnswerFromGraph answered with no store")
	}
	if _, _, ok := m.Adjudicate([]string{"a", "b"}); ok {
		t.Error("Adjudicate reported a verdict with no graph")
	}
	if _, ok := m.BlastRadius([]string{"a.go"}, 2); ok {
		t.Error("BlastRadius reported impact with no graph")
	}
	if n := m.PropagateConfidence("file:a.go", -0.1, 0.5, 2); n != 0 {
		t.Errorf("PropagateConfidence softened %d beliefs with no graph", n)
	}
	if m.Recall("x", 5) != nil || m.Skills() != nil {
		t.Error("a read returned data with no store")
	}
}

// TestGraphReadsWorkThroughTheGateway — the three reasoning calls used to be
// three separate downcasts to the concrete graph in the orchestrator. They must
// still reach a real graph through the one seam that replaced them.
func TestGraphReadsWorkThroughTheGateway(t *testing.T) {
	s := newStore(t)
	m := New(s)
	if _, _, ok := m.Adjudicate([]string{"a", "b"}); !ok {
		t.Error("Adjudicate could not reach the graph behind a real store")
	}
	if _, ok := m.BlastRadius([]string{"main.go"}, 2); !ok {
		t.Error("BlastRadius could not reach the graph behind a real store")
	}
}
