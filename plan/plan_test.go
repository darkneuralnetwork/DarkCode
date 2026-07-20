package plan

import (
	"strings"
	"testing"

	"github.com/darkcode/core"
)

func sampleGraph() *Graph {
	g, err := Parse(`{"summary":"Build it in two steps.","tasks":[
		{"name":"research","goal":"find the API shape","agent":"research","deps":[],"est_complexity":2},
		{"name":"impl","goal":"write the client","agent":"worker","deps":["research"],"acceptance":["client compiles"],"artifacts":["client.go"],"est_complexity":5},
		{"name":"test","goal":"add tests","agent":"qa","deps":["impl"]}
	]}`, "build an API client")
	if err != nil {
		panic(err)
	}
	return g
}

func TestParseObjectWithSummary(t *testing.T) {
	g := sampleGraph()
	if g.Summary != "Build it in two steps." {
		t.Errorf("summary = %q", g.Summary)
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(g.Nodes))
	}
	if g.Nodes[0].ID != "T1" || g.Nodes[1].ID != "T2" || g.Nodes[2].ID != "T3" {
		t.Errorf("ids = %s,%s,%s", g.Nodes[0].ID, g.Nodes[1].ID, g.Nodes[2].ID)
	}
	if len(g.Nodes[1].Deps) != 1 || g.Nodes[1].Deps[0] != "T1" {
		t.Errorf("T2 deps = %v, want [T1]", g.Nodes[1].Deps)
	}
	if g.Nodes[1].Artifacts[0] != "client.go" {
		t.Errorf("artifacts = %v", g.Nodes[1].Artifacts)
	}
}

func TestParseBareArrayInProseAndFence(t *testing.T) {
	out := "Here is my plan after analysis:\n```json\n" +
		`[{"name":"a","goal":"do a","agent":"worker","dependencies":[]},{"name":"b","goal":"do b","agent":"qa","dependencies":["a"]}]` +
		"\n```\nDone."
	g, err := Parse(out, "goal")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 || g.Nodes[1].Deps[0] != "T1" {
		t.Fatalf("unexpected parse: %+v", g.Nodes)
	}
}

func TestParseAnalysisProseThenJSON(t *testing.T) {
	out := "<analysis>\nStep 1 think. Step 2 think more.\n</analysis>\n" +
		`{"summary":"s","tasks":[{"name":"only","goal":"do it","agent":"worker"}]}`
	g, err := Parse(out, "goal")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 1 {
		t.Fatalf("nodes = %d", len(g.Nodes))
	}
}

func TestParseGarbage(t *testing.T) {
	for _, in := range []string{"", "no plan here", "[broken", "{}", `{"tasks":[]}`} {
		if _, err := Parse(in, "g"); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
}

func TestValidateCatchesCycleAndBadDeps(t *testing.T) {
	g := &Graph{Nodes: []*Node{
		{ID: "T1", Goal: "a", Deps: []string{"T2"}},
		{ID: "T2", Goal: "b", Deps: []string{"T1"}},
	}}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("want cycle error, got %v", err)
	}
	g2 := &Graph{Nodes: []*Node{{ID: "T1", Goal: "a", Deps: []string{"TX"}}}}
	if err := g2.Validate(); err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Errorf("want unknown-dep error, got %v", err)
	}
}

func TestWavesAndToDAG(t *testing.T) {
	g := sampleGraph()
	waves := g.Waves()
	if len(waves) != 3 {
		t.Fatalf("waves = %d, want 3", len(waves))
	}
	d := g.ToDAG()
	if d.NodeCount() != 3 {
		t.Fatalf("dag nodes = %d", d.NodeCount())
	}
	ready := d.ReadyNodes()
	if len(ready) != 1 || ready[0].ID != "T1" {
		t.Fatalf("ready = %v", ready)
	}
	// Complexity-driven placement: est_complexity 2 demotes the research
	// node to the fast tier (cheap model), est 5 keeps worker on coding.
	n1, _ := d.GetNode("T1")
	if n1.ModelTier != core.ModelTierFast {
		t.Errorf("T1 tier = %s, want fast", n1.ModelTier)
	}
	n2, _ := d.GetNode("T2")
	if n2.ModelTier != core.ModelTierCoding {
		t.Errorf("T2 tier = %s, want coding", n2.ModelTier)
	}
}

func TestSyncFromRoundTrip(t *testing.T) {
	g := sampleGraph()
	d := g.ToDAG()
	d.MarkCompleted("T1")
	d.SetOutput("T1", "the API is REST")
	d.UpdateStatus("T2", core.TaskFailed)
	d.SetError("T2", "compile error")
	g.SyncFrom(d)
	if g.Node("T1").Status != core.TaskCompleted || g.Node("T1").Output != "the API is REST" {
		t.Errorf("T1 not synced: %+v", g.Node("T1"))
	}
	if g.Node("T2").Status != core.TaskFailed || g.Node("T2").Error != "compile error" {
		t.Errorf("T2 not synced: %+v", g.Node("T2"))
	}
}

func TestSerializationRoundTrip(t *testing.T) {
	g := sampleGraph()
	b := g.Bytes()
	g2, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Nodes) != 3 || g2.Nodes[2].ID != "T3" || g2.Goal != g.Goal {
		t.Fatalf("round trip mismatch: %+v", g2)
	}
}

func TestRenderers(t *testing.T) {
	g := sampleGraph()
	g.Node("T1").Status = core.TaskCompleted
	g.Node("T2").Status = core.TaskFailed
	g.Node("T2").Error = "boom"

	wf := RenderWorkflow(g)
	if !strings.Contains(wf, "- [x] T1:") {
		t.Errorf("workflow missing done box:\n%s", wf)
	}
	if !strings.Contains(wf, "- [ ] T2:") || !strings.Contains(wf, "failed") {
		t.Errorf("workflow missing failed annotation:\n%s", wf)
	}

	mm := RenderMermaid(g)
	for _, want := range []string{"graph TD", "T1 --> T2", "class T1 done", "class T2 failed"} {
		if !strings.Contains(mm, want) {
			t.Errorf("mermaid missing %q:\n%s", want, mm)
		}
	}

	md := RenderMarkdown(g)
	for _, want := range []string{"# Implementation Plan", "## Proposed Changes", "T2 — impl", "client.go", "## Architecture"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}

	pv := Preview(g)
	for _, want := range []string{"Proposed Plan", "3 task(s) in 3 execution wave(s)", "Reply **approve**"} {
		if !strings.Contains(pv, want) {
			t.Errorf("preview missing %q:\n%s", want, pv)
		}
	}
}
