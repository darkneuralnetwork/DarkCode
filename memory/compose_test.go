package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// newComposeKG builds a real KnowledgeGraph for these tests. A hand-rolled
// fake would have to track core.KnowledgeGraphStore's full surface, and the
// real one is cheap over a temp dir.
func newComposeKG(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	return kg
}

func addKGNode(t *testing.T, kg *KnowledgeGraph, id, label string, typ core.KGNodeType, detail, prov string, conf float64) *core.KGNode {
	t.Helper()
	n := &core.KGNode{
		ID: id, Label: label, Type: typ, Provenance: prov, Confidence: conf,
		Properties: map[string]string{"detail": detail},
		CreatedAt:  time.Now(), LastSeen: time.Now(),
	}
	if err := kg.AddNode(n); err != nil {
		t.Fatalf("AddNode(%s): %v", id, err)
	}
	return n
}

// TestComposeAnswersFromLiveFacts is the whole point of composition: the answer
// is assembled from the graph at question time, so it reflects the graph's
// current state rather than the text of some earlier reply.
func TestComposeAnswersFromLiveFacts(t *testing.T) {
	sys := newTestSystem(t)
	kg := newComposeKG(t)
	addKGNode(t, kg, "n1", "retryBackoff", core.KGNodeSymbol,
		"exponential backoff helper for the retry layer", "llm/retry.go:44", 0.9)
	addKGNode(t, kg, "n2", "retry", core.KGNodeDecision,
		"retry layer uses exponential backoff capped at 30s", "task_88", 0.85)

	r := NewHybridRetriever(sys, nil)
	ca, ok := r.ComposeAnswer(kg, "what is the retry backoff")
	if !ok {
		t.Fatal("expected an answer composed from the graph")
	}
	if !strings.Contains(ca.Text, "retryBackoff") || !strings.Contains(ca.Text, "llm/retry.go:44") {
		t.Errorf("composed answer should carry the fact and its provenance:\n%s", ca.Text)
	}
	if len(ca.SourceNodeIDs) < 2 {
		t.Errorf("SourceNodeIDs = %v, want the backing facts so a rejection can demote them", ca.SourceNodeIDs)
	}
}

// TestComposeDeclinesWhenEvidenceDoesNotCoverTheQuestion is the guard that
// makes an LLM-free answer trustworthy rather than merely cheap. Assembling
// something confident out of facts that never mention half the question is
// worse than escalating.
func TestComposeDeclinesWhenEvidenceDoesNotCoverTheQuestion(t *testing.T) {
	sys := newTestSystem(t)
	kg := newComposeKG(t)
	addKGNode(t, kg, "n1", "retryBackoff", core.KGNodeSymbol,
		"exponential backoff helper", "llm/retry.go:44", 0.9)
	addKGNode(t, kg, "n2", "retry", core.KGNodeDecision,
		"retry layer uses exponential backoff", "task_88", 0.85)

	r := NewHybridRetriever(sys, nil)
	// "kubernetes" and "deployment" appear in no stored fact.
	if ca, ok := r.ComposeAnswer(kg, "how does the kubernetes deployment retry backoff"); ok {
		t.Errorf("composed an answer that does not cover the question:\n%s", ca.Text)
	}
}

// TestComposeRefusesCommands. A command asks for the world to change, and no
// amount of stored knowledge about the world satisfies it.
func TestComposeRefusesCommands(t *testing.T) {
	sys := newTestSystem(t)
	kg := newComposeKG(t)
	addKGNode(t, kg, "n1", "retryBackoff", core.KGNodeSymbol, "backoff helper", "llm/retry.go:44", 0.9)
	addKGNode(t, kg, "n2", "retry", core.KGNodeDecision, "uses backoff", "task_88", 0.85)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.ComposeAnswer(kg, "fix the retry backoff helper"); ok {
		t.Error("a command must never be answered from stored knowledge")
	}
}

// TestComposeSkipsDemotedFacts. Write-back governance demotes a fact when its
// answer was rejected; it must not come back through a different door.
func TestComposeSkipsDemotedFacts(t *testing.T) {
	sys := newTestSystem(t)
	kg := newComposeKG(t)
	addKGNode(t, kg, "n1", "retryBackoff", core.KGNodeSymbol, "backoff helper", "llm/retry.go:44", 0.2)
	addKGNode(t, kg, "n2", "retry", core.KGNodeDecision, "uses backoff", "task_88", 0.15)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.ComposeAnswer(kg, "what is the retry backoff"); ok {
		t.Error("demoted facts must not be served through composition")
	}
}

// TestComposeNeedsMoreThanOneFact. One matching node is a coincidence as often
// as it is an answer.
func TestComposeNeedsMoreThanOneFact(t *testing.T) {
	sys := newTestSystem(t)
	kg := newComposeKG(t)
	addKGNode(t, kg, "n1", "retryBackoff", core.KGNodeSymbol, "backoff helper", "llm/retry.go:44", 0.9)

	r := NewHybridRetriever(sys, nil)
	if _, ok := r.ComposeAnswer(kg, "what is the retry backoff"); ok {
		t.Error("a single matching node should not be enough to answer")
	}
}

// TestComposeReflectsGraphChanges is the structural property the cache cannot
// have. Change the graph, ask the same question, get a different answer — with
// no TTL, no invalidation and no re-ask, because nothing was ever stored.
func TestComposeReflectsGraphChanges(t *testing.T) {
	sys := newTestSystem(t)
	kg := newComposeKG(t)
	addKGNode(t, kg, "n1", "authTimeout", core.KGNodeDecision,
		"auth timeout is 30 seconds", "task_1", 0.9)
	addKGNode(t, kg, "n2", "auth", core.KGNodeSymbol,
		"auth timeout constant", "auth/config.go:12", 0.9)

	r := NewHybridRetriever(sys, nil)
	first, ok := r.ComposeAnswer(kg, "what is the auth timeout")
	if !ok {
		t.Fatal("expected a first answer")
	}
	if !strings.Contains(first.Text, "30 seconds") {
		t.Fatalf("first answer lost the fact:\n%s", first.Text)
	}

	// The world moves on: the decision is superseded in the graph.
	addKGNode(t, kg, "n1", "authTimeout", core.KGNodeDecision,
		"auth timeout is 5 seconds", "task_2", 0.95)

	second, ok := r.ComposeAnswer(kg, "what is the auth timeout")
	if !ok {
		t.Fatal("expected a second answer")
	}
	if strings.Contains(second.Text, "30 seconds") {
		t.Errorf("composed answer replayed the superseded fact:\n%s", second.Text)
	}
	if !strings.Contains(second.Text, "5 seconds") {
		t.Errorf("composed answer did not pick up the current fact:\n%s", second.Text)
	}
}
