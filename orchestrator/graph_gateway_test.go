package orchestrator

import (
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
	"github.com/darkcode/recall"
)

// TestKernelGraphWritesGoThroughTheGateway.
//
// The graph sync's call sites still speak core.KnowledgeGraphStore, so the
// static boundary count does not see them as gateway writes. What makes them
// gateway writes is k.graph(): it hands out recall's writer, not the raw store.
// That is the whole of the claim, and until this test nothing checked it — the
// adapter was covered in package recall, the kernel's use of it was not.
//
// Returning k.memory.KG() here would be invisible: every write would still
// land, the tests would still pass, and the manager would silently stop owning
// placement.
func TestKernelGraphWritesGoThroughTheGateway(t *testing.T) {
	deps := newTestKernel(t, nil)

	g := deps.Kernel.graph()
	if _, ok := g.(*recall.GraphWriter); !ok {
		t.Fatalf("kernel writes the graph directly (%T), bypassing the memory gateway", g)
	}
	if g == deps.Memory.KG() {
		t.Fatal("kernel graph writes reach the raw store")
	}

	// And the writer is a real one, not a decorative wrapper: a node written
	// through it is in the store afterwards.
	if err := g.AddNode(&core.KGNode{ID: "file:gateway.go", Label: "gateway.go", Type: core.KGNodeFile, Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := deps.Memory.KG().GetNode("file:gateway.go"); !ok {
		t.Error("the write did not reach the store")
	}
}

// TestKernelGraphFollowsTheInstalledManager — SetRecall swapping the gateway
// must move the graph writes with it. A cached writer would keep feeding the
// manager that was replaced.
func TestKernelGraphFollowsTheInstalledManager(t *testing.T) {
	deps := newTestKernel(t, nil)

	other, err := memory.NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Shutdown)
	m, err := recall.New(other)
	if err != nil {
		t.Fatal(err)
	}
	deps.Kernel.SetRecall(m)

	if err := deps.Kernel.graph().AddNode(&core.KGNode{ID: "file:swapped.go", Label: "swapped.go", Type: core.KGNodeFile, Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := other.KG().GetNode("file:swapped.go"); !ok {
		t.Error("the write went to the replaced gateway's store")
	}
}
