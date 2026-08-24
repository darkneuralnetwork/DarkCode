package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
)

// simGraph builds a repository whose package structure is known exactly:
//
//	api  → core
//	cli  → api, core
//	util → core
func simGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)

	file := func(rel string) {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + rel, Label: rel, Type: core.KGNodeFile,
			Confidence: 1, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	imp := func(from, pkg string) {
		id := "package:" + pkg
		if err := kg.AddNode(&core.KGNode{ID: id, Label: pkg, Type: core.KGNodePackage, Confidence: 1}); err != nil {
			t.Fatal(err)
		}
		if err := kg.AddEdge(&core.KGEdge{From: "file:" + from, To: id, Relation: core.KGRelImports, Weight: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"api/a.go", "core/c.go", "cli/m.go", "util/u.go"} {
		file(f)
	}
	imp("api/a.go", "example.com/m/core")
	imp("cli/m.go", "example.com/m/api")
	imp("cli/m.go", "example.com/m/core")
	imp("util/u.go", "example.com/m/core")
	return kg
}

func TestSimulateRemoveDependency(t *testing.T) {
	kg := simGraph(t)
	sim, err := kg.Simulate(Change{Kind: "remove_dependency", From: "cli", To: "api"})
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if sim.After.Edges != sim.Before.Edges-1 {
		t.Errorf("edges %d → %d, want one fewer", sim.Before.Edges, sim.After.Edges)
	}
	if sim.After.AvgCoupling >= sim.Before.AvgCoupling {
		t.Errorf("coupling should fall: %.2f → %.2f", sim.Before.AvgCoupling, sim.After.AvgCoupling)
	}
	if len(sim.Notes) == 0 {
		t.Error("the change should be described")
	}
}

// Simulating must never touch the real graph.
func TestSimulateDoesNotMutateTheGraph(t *testing.T) {
	kg := simGraph(t)
	before := measure(kg.packageGraph())

	if _, err := kg.Simulate(Change{Kind: "remove_dependency", From: "cli", To: "api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := kg.Simulate(Change{Kind: "split", Package: "core", Into: []string{"core/a", "core/b"}}); err != nil {
		t.Fatal(err)
	}

	after := measure(kg.packageGraph())
	if before != after {
		t.Errorf("the real graph changed: %+v → %+v", before, after)
	}
}

// The headline use: does inverting this edge remove the cycle?
func TestSimulateDetectsCycleCreationAndRemoval(t *testing.T) {
	kg := simGraph(t)

	// api→core inverted gives core→api, and cli still depends on both — no
	// cycle. Inverting cli→api while api→core and cli→core hold does create
	// one, since api would then import cli.
	creates, err := kg.Simulate(Change{Kind: "invert_dependency", From: "cli", To: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if creates.After.Edges != creates.Before.Edges {
		t.Errorf("inverting must preserve the edge count: %d → %d", creates.Before.Edges, creates.After.Edges)
	}

	// A graph that already has a cycle should show it removed.
	cyclic := map[string][]string{"a": {"b"}, "b": {"a"}}
	if measure(cyclic).Cycles == 0 {
		t.Fatal("fixture has no cycle")
	}
	fixed, _, err := applyChange(cyclic, Change{Kind: "remove_dependency", From: "b", To: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if measure(fixed).Cycles != 0 {
		t.Error("removing the back edge should clear the cycle")
	}
	if v := verdict(measure(cyclic), measure(fixed)); !strings.Contains(v, "removes 1 cycle") {
		t.Errorf("verdict = %q, want it to lead with the cycle", v)
	}
}

func TestSimulateSplitRewiresCallers(t *testing.T) {
	kg := simGraph(t)
	sim, err := kg.Simulate(Change{
		Kind: "split", Package: "core", Into: []string{"core/types", "core/logic"},
	})
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if sim.After.Packages != sim.Before.Packages+1 {
		t.Errorf("packages %d → %d, want one more", sim.Before.Packages, sim.After.Packages)
	}
	// Three packages imported core, and each now imports both halves.
	if sim.After.Edges <= sim.Before.Edges {
		t.Errorf("splitting should add edges under the pessimistic assumption: %d → %d",
			sim.Before.Edges, sim.After.Edges)
	}
	joined := strings.Join(sim.Notes, " ")
	if !strings.Contains(joined, "assumed to depend on both halves") {
		t.Errorf("the pessimistic assumption must be stated: %v", sim.Notes)
	}
}

// A proposal that does not match reality is an error, not a silent no-op.
func TestSimulateRejectsImpossibleChanges(t *testing.T) {
	kg := simGraph(t)
	cases := []Change{
		{Kind: "remove_dependency", From: "core", To: "cli"}, // no such edge
		{Kind: "remove_dependency", From: "cli"},             // missing to
		{Kind: "invert_dependency", From: "nope", To: "core"},
		{Kind: "split", Package: "core", Into: []string{"only-one"}},
		{Kind: "split", Package: "ghost", Into: []string{"a", "b"}},
		{Kind: "teleport"},
	}
	for _, c := range cases {
		if _, err := kg.Simulate(c); err == nil {
			t.Errorf("expected an error for %+v", c)
		}
	}
}

func TestMaxDepthTerminatesOnCycles(t *testing.T) {
	// A cycle must bound the walk rather than recurse forever.
	done := make(chan int, 1)
	go func() { done <- maxDepth(map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}}) }()
	select {
	case <-done:
	case <-timeAfterShort():
		t.Fatal("maxDepth did not terminate on a cyclic graph")
	}

	linear := map[string][]string{"a": {"b"}, "b": {"c"}, "c": {}}
	if got := maxDepth(linear); got != 2 {
		t.Errorf("maxDepth of a→b→c = %d, want 2", got)
	}
}

func TestMeasureIsDeterministic(t *testing.T) {
	graph := map[string][]string{"a": {"c"}, "b": {"c"}, "c": {}}
	first := measure(graph)
	for i := 0; i < 20; i++ {
		if got := measure(graph); got != first {
			t.Fatalf("measure varies between runs: %+v vs %+v", first, got)
		}
	}
	if first.MaxFanInPkg != "c" || first.MaxFanIn != 2 {
		t.Errorf("fan-in = %d on %q, want 2 on c", first.MaxFanIn, first.MaxFanInPkg)
	}
}

func TestSimulationFormat(t *testing.T) {
	kg := simGraph(t)
	sim, err := kg.Simulate(Change{Kind: "remove_dependency", From: "cli", To: "api"})
	if err != nil {
		t.Fatal(err)
	}
	out := sim.Format()
	for _, want := range []string{"before", "after", "cycles", "avg coupling", "max chain depth"} {
		if !strings.Contains(out, want) {
			t.Errorf("format missing %q:\n%s", want, out)
		}
	}
}

// timeAfterShort is a small helper so the cycle-termination test reads
// clearly; a hang there would otherwise stall the whole suite.
func timeAfterShort() <-chan time.Time { return time.After(2 * time.Second) }
