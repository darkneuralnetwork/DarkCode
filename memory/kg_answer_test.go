package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/core"
)

// seedCodeKG builds a KG holding the typed facts the deterministic code index
// would write: one symbol with provenance, one package with two importers,
// and one past task that used tools.
func seedCodeKG(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	t.Cleanup(kg.Shutdown)
	now := time.Now()

	mustAdd := func(n *core.KGNode) {
		t.Helper()
		if err := kg.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(&core.KGNode{ID: "file:router/router.go", Label: "router/router.go", Type: core.KGNodeFile, Provenance: "router/router.go", Confidence: 1, LastSeen: now})
	mustAdd(&core.KGNode{ID: "file:orchestrator/kernel.go", Label: "orchestrator/kernel.go", Type: core.KGNodeFile, Provenance: "orchestrator/kernel.go", Confidence: 1, LastSeen: now})
	mustAdd(&core.KGNode{
		ID: "symbol:Route@router/router.go", Label: "Route", Type: core.KGNodeSymbol,
		Properties: map[string]string{"kind": "function", "receiver": "*Router", "references": "6", "origin": "code_index"},
		Provenance: "router/router.go:95", Confidence: 1, LastSeen: now,
	})
	mustAdd(&core.KGNode{ID: "package:github.com/darkcode/router", Label: "github.com/darkcode/router", Type: core.KGNodePackage, Confidence: 1, LastSeen: now})

	mustEdge := func(e *core.KGEdge) {
		t.Helper()
		if err := kg.AddEdge(e); err != nil {
			t.Fatal(err)
		}
	}
	mustEdge(&core.KGEdge{From: "file:router/router.go", To: "symbol:Route@router/router.go", Relation: core.KGRelDefines, Weight: 1, Provenance: "router/router.go:95"})
	mustEdge(&core.KGEdge{From: "file:orchestrator/kernel.go", To: "package:github.com/darkcode/router", Relation: core.KGRelImports, Weight: 1})
	mustEdge(&core.KGEdge{From: "file:router/router.go", To: "package:github.com/darkcode/router", Relation: core.KGRelImports, Weight: 1})

	// Task activity: an auth task that used two tools.
	mustAdd(&core.KGNode{ID: "task:fix auth token refresh", Label: "fix auth token refresh", Type: core.KGNodeTask})
	mustAdd(&core.KGNode{ID: "tool:file_write", Label: "file_write", Type: core.KGNodeTool})
	mustAdd(&core.KGNode{ID: "tool:terminal", Label: "terminal", Type: core.KGNodeTool})
	mustEdge(&core.KGEdge{From: "task:fix auth token refresh", To: "tool:file_write", Relation: core.KGRelUsedBy, Weight: 1})
	mustEdge(&core.KGEdge{From: "task:fix auth token refresh", To: "tool:terminal", Relation: core.KGRelUsedBy, Weight: 1})

	return kg
}

func TestAnswerFromGraph_Definition(t *testing.T) {
	kg := seedCodeKG(t)

	ans, ok := AnswerFromGraph(kg, "where is Route defined?")
	if !ok {
		t.Fatal("expected a graph answer for a definition question")
	}
	if !strings.Contains(ans.Text, "router/router.go:95") {
		t.Fatalf("answer missing citation: %s", ans.Text)
	}
	if ans.Confidence.Score < 0.9 {
		t.Fatalf("sourced definition answer should be high-confidence, got %f", ans.Confidence.Score)
	}
	if len(ans.Confidence.Provenance) == 0 {
		t.Fatal("expected provenance on the confidence signal")
	}

	// Receiver-qualified form should also resolve.
	if _, ok := AnswerFromGraph(kg, "where is Router.Route defined?"); !ok {
		t.Fatal("receiver-qualified lookup failed")
	}
}

func TestAnswerFromGraph_Importers(t *testing.T) {
	kg := seedCodeKG(t)

	ans, ok := AnswerFromGraph(kg, "which files import github.com/darkcode/router?")
	if !ok {
		t.Fatal("expected a graph answer for an imports question")
	}
	if !strings.Contains(ans.Text, "orchestrator/kernel.go") || !strings.Contains(ans.Text, "router/router.go") {
		t.Fatalf("answer missing importer files: %s", ans.Text)
	}

	// Trailing-fragment package match ("router" for the full path).
	if _, ok := AnswerFromGraph(kg, "who imports router?"); !ok {
		t.Fatal("package fragment lookup failed")
	}
}

func TestAnswerFromGraph_References(t *testing.T) {
	kg := seedCodeKG(t)
	ans, ok := AnswerFromGraph(kg, "who references Route?")
	if !ok {
		t.Fatal("expected a graph answer for a references question")
	}
	if !strings.Contains(ans.Text, "6 other file(s)") {
		t.Fatalf("answer missing reference count: %s", ans.Text)
	}
}

func TestAnswerFromGraph_ToolsForTopic(t *testing.T) {
	kg := seedCodeKG(t)
	ans, ok := AnswerFromGraph(kg, "what tools did we use for the auth token work?")
	if !ok {
		t.Fatal("expected a graph answer for a tools-activity question")
	}
	if !strings.Contains(ans.Text, "file_write") || !strings.Contains(ans.Text, "terminal") {
		t.Fatalf("answer missing tools: %s", ans.Text)
	}
}

func TestAnswerFromGraph_MissesEscalate(t *testing.T) {
	kg := seedCodeKG(t)

	// Not graph-shaped at all → miss.
	if _, ok := AnswerFromGraph(kg, "please refactor the auth flow to use JWTs"); ok {
		t.Fatal("non-structural question must not be answered by the graph")
	}
	// Graph-shaped but unknown symbol → miss (escalate, don't guess).
	if _, ok := AnswerFromGraph(kg, "where is FooBarBaz defined?"); ok {
		t.Fatal("unknown symbol must escalate, not answer")
	}
	// Unsourced concept facts must never answer.
	_ = kg.RecordWordRelations("authentication token refresh logic")
	if _, ok := AnswerFromGraph(kg, "where is authentication defined?"); ok {
		t.Fatal("concept co-occurrence must not answer a definition question")
	}
}

// seedFixDecisionKG adds the typed facts orchestrator/reflection.go's
// populateKnowledgeGraph writes (Phase D): a problem task, a fix task+node
// linked by fixed_by, and a decision node.
func seedFixDecisionKG(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	t.Cleanup(kg.Shutdown)
	now := time.Now()

	mustAdd := func(n *core.KGNode) {
		t.Helper()
		if err := kg.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	mustEdge := func(e *core.KGEdge) {
		t.Helper()
		if err := kg.AddEdge(e); err != nil {
			t.Fatal(err)
		}
	}

	problemID := "task:fix the nil pointer crash in the auth middleware"
	fixID := "fix:fix the nil pointer crash in the auth middleware"
	taskID := problemID // the successful retry re-used the same goal text

	mustAdd(&core.KGNode{ID: problemID, Label: "fix the nil pointer crash in the auth middleware", Type: core.KGNodeTask, Properties: map[string]string{"outcome": "failure"}, CreatedAt: now})
	mustAdd(&core.KGNode{
		ID: fixID, Label: "fix the nil pointer crash in the auth middleware", Type: core.KGNodeFix,
		Properties: map[string]string{"resolution": "added a nil check before dereferencing the session pointer", "lessons": "Fixed a previously failing task"},
		Provenance: taskID, Confidence: 0.8, LastSeen: now,
	})
	mustEdge(&core.KGEdge{From: problemID, To: fixID, Relation: core.KGRelFixedBy, Weight: 1})

	decisionID := "decision:choose between REST and GraphQL for the public API"
	mustAdd(&core.KGNode{
		ID: decisionID, Label: "choose between REST and GraphQL for the public API", Type: core.KGNodeDecision,
		Properties: map[string]string{"rationale": "Decided an approach for the public API using strategy \"dag\""},
		Provenance: "task:choose between REST and GraphQL for the public API", Confidence: 0.8, LastSeen: now,
	})

	return kg
}

func TestAnswerFromGraph_FixHistory(t *testing.T) {
	kg := seedFixDecisionKG(t)

	ans, ok := AnswerFromGraph(kg, "how did we fix the nil pointer crash in auth?")
	if !ok {
		t.Fatal("expected a graph answer for a fix-history question")
	}
	if !strings.Contains(ans.Text, "nil check") {
		t.Fatalf("answer missing resolution: %s", ans.Text)
	}
	if ans.Confidence.Score <= 0 || ans.Confidence.Score >= 0.85 {
		t.Fatalf("fix facts are episodic-sourced, not code-verified — expected a mid confidence, got %f", ans.Confidence.Score)
	}
	if len(ans.Confidence.Provenance) == 0 {
		t.Fatal("expected provenance on the fix-history answer")
	}
}

func TestAnswerFromGraph_DecisionHistory(t *testing.T) {
	kg := seedFixDecisionKG(t)

	ans, ok := AnswerFromGraph(kg, "why did we decide REST vs GraphQL for the API?")
	if !ok {
		t.Fatal("expected a graph answer for a decision-history question")
	}
	if !strings.Contains(ans.Text, "Decided an approach") {
		t.Fatalf("answer missing rationale: %s", ans.Text)
	}
}

func TestAnswerFromGraph_FixDecisionMissesEscalate(t *testing.T) {
	kg := seedFixDecisionKG(t)

	if _, ok := AnswerFromGraph(kg, "how did we fix the completely unrelated payment timeout bug?"); ok {
		t.Fatal("a fix question with no matching fact must escalate, not answer")
	}
	if _, ok := AnswerFromGraph(kg, "why did we decide to use kubernetes?"); ok {
		t.Fatal("a decision question with no matching fact must escalate, not answer")
	}
}

// TestAnswerFromGraph_FixHistoryUsesMinConfidence is the write-back
// governance regression test (local-first upgrade Phase D hardening): when
// a query matches two fix facts of different confidence — one full-strength,
// one demoted by a prior rejection — the answer's overall confidence must be
// the WEAKER of the two, never an average or the stronger one. A demoted
// fact must be able to single-handedly pull a multi-fact answer's confidence
// below the cascade's answer threshold.
func TestAnswerFromGraph_FixHistoryUsesMinConfidence(t *testing.T) {
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatalf("NewKnowledgeGraph: %v", err)
	}
	t.Cleanup(kg.Shutdown)
	now := time.Now()

	mustAdd := func(n *core.KGNode) {
		t.Helper()
		if err := kg.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	mustEdge := func(e *core.KGEdge) {
		t.Helper()
		if err := kg.AddEdge(e); err != nil {
			t.Fatal(err)
		}
	}

	// Two independent problem→fix pairs, both about "database connection
	// pool exhaustion" so a single query's topic tokens overlap both.
	mustAdd(&core.KGNode{ID: "task:database connection pool exhaustion on checkout", Label: "database connection pool exhaustion on checkout", Type: core.KGNodeTask})
	mustAdd(&core.KGNode{
		ID: "fix:database connection pool exhaustion on checkout", Label: "database connection pool exhaustion on checkout", Type: core.KGNodeFix,
		Properties: map[string]string{"resolution": "raised the pool size and added a timeout"},
		Provenance: "task:database connection pool exhaustion on checkout", Confidence: 0.84, LastSeen: now,
	})
	mustEdge(&core.KGEdge{From: "task:database connection pool exhaustion on checkout", To: "fix:database connection pool exhaustion on checkout", Relation: core.KGRelFixedBy, Weight: 1})

	mustAdd(&core.KGNode{ID: "task:database connection pool exhaustion on batch import", Label: "database connection pool exhaustion on batch import", Type: core.KGNodeTask})
	mustAdd(&core.KGNode{
		ID: "fix:database connection pool exhaustion on batch import", Label: "database connection pool exhaustion on batch import", Type: core.KGNodeFix,
		// Demoted below the floor by a prior rejection (write-back governance).
		Properties: map[string]string{"resolution": "closed a connection leak in the import job"},
		Provenance: "task:database connection pool exhaustion on batch import", Confidence: 0.3, LastSeen: now,
	})
	mustEdge(&core.KGEdge{From: "task:database connection pool exhaustion on batch import", To: "fix:database connection pool exhaustion on batch import", Relation: core.KGRelFixedBy, Weight: 1})

	ans, ok := AnswerFromGraph(kg, "how did we fix the database connection pool exhaustion?")
	if !ok {
		t.Fatal("expected a graph answer matching both fix facts")
	}
	if len(ans.SourceNodeIDs) != 2 {
		t.Fatalf("expected both fix facts to contribute, got SourceNodeIDs=%v", ans.SourceNodeIDs)
	}
	if ans.Confidence.Score != 0.3 {
		t.Fatalf("Confidence.Score = %v, want the minimum (0.3) across matched facts, not an average or the max", ans.Confidence.Score)
	}
}
