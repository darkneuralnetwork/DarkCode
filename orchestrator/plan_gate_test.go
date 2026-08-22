package orchestrator

import (
	"context"
	"strings"
	"testing"
)

func TestClassifyPlanDecision(t *testing.T) {
	cases := []struct {
		msg  string
		want planDecision
	}{
		{"approve", planApprove},
		{"Approve!", planApprove},
		{"yes", planApprove},
		{"y", planApprove},
		{"ok go ahead", planApprove},
		{"looks good", planApprove},
		{"LGTM", planApprove},
		{"proceed", planApprove},
		{"run it", planApprove},

		{"reject", planReject},
		{"no", planReject},
		{"cancel", planReject},
		{"nevermind", planReject},
		{"forget it", planReject},

		// Anything with substance is feedback — even when it starts with an
		// approve/reject word.
		{"yes but add tests first", planFeedback},
		{"no, use python instead", planFeedback},
		{"ok but remove T3", planFeedback},
		{"split T2 into two tasks", planFeedback},
		{"the research task should come first", planFeedback},
		{"can you add a security review step", planFeedback},
		{"", planFeedback},
	}
	for _, c := range cases {
		if got := classifyPlanDecision(c.msg); got != c.want {
			t.Errorf("classifyPlanDecision(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestPlanApprovalRequired(t *testing.T) {
	cases := []struct {
		cfg   string
		depth PlanDepth
		want  bool
	}{
		{"always", PlanDepthLight, true},
		{"always", PlanDepthDeep, true},
		{"auto", PlanDepthLight, false},
		{"auto", PlanDepthDeep, true},
		{"never", PlanDepthDeep, false},
		{"", PlanDepthDeep, false}, // zero-value default: pre-gate behavior
	}
	for _, c := range cases {
		if got := planApprovalRequired(c.cfg, c.depth); got != c.want {
			t.Errorf("planApprovalRequired(%q, %s) = %v, want %v", c.cfg, c.depth, got, c.want)
		}
	}
}

func TestDecidePlanDepth(t *testing.T) {
	if d := decidePlanDepth("simple-ish task", 6, false, ""); d != PlanDepthLight {
		t.Errorf("moderate no-project = %s, want light", d)
	}
	if d := decidePlanDepth("complex task", 8, false, ""); d != PlanDepthDeep {
		t.Errorf("complexity 8 = %s, want deep", d)
	}
	if d := decidePlanDepth("project task", 6, true, ""); d != PlanDepthDeep {
		t.Errorf("project complexity 6 = %s, want deep", d)
	}
	if d := decidePlanDepth("anything", 9, true, "light"); d != PlanDepthLight {
		t.Errorf("override light = %s, want light", d)
	}
	if d := decidePlanDepth("anything", 3, false, "deep"); d != PlanDepthDeep {
		t.Errorf("override deep = %s, want deep", d)
	}
}

// TestPlanGateEndToEnd drives the full conversational flow through
// Kernel.Execute: proposal → revision feedback → approval → execution.
func TestPlanGateEndToEnd(t *testing.T) {
	revisedJSON := `[{"name":"task1","goal":"write the function","agent":"worker","deps":[]},{"name":"task2","goal":"write the tests","agent":"qa","deps":["task1"]},{"name":"task3","goal":"security scan","agent":"security","deps":["task2"]}]`
	client := &fakeLLMClient{name: "primary", responses: []string{
		testPlanJSON,          // 1: initial (light) plan
		revisedJSON,           // 2: revision incorporating feedback
		"func done",           // 3: worker T1
		"tests done",          // 4: qa T2
		"scan done",           // 5: security T3
		"final merged answer", // 6: merge synthesis
	}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetPlanControls("always", "light")

	// Multi-step goal so the trivial path doesn't swallow it.
	goal := "implement the feature and then add tests step by step"
	out, err := deps.Kernel.Execute(context.Background(), goal)
	if err != nil {
		t.Fatalf("Execute(goal): %v", err)
	}
	if !strings.Contains(out, "Proposed Plan") || !strings.Contains(out, "Reply **approve**") {
		t.Fatalf("first turn should be a plan proposal, got:\n%s", out)
	}
	if !deps.Kernel.PlanAwaitingApproval() {
		t.Fatal("PlanAwaitingApproval = false after proposal")
	}

	// Feedback turn → revised proposal, still pending.
	out, err = deps.Kernel.Execute(context.Background(), "also add a security scan step")
	if err != nil {
		t.Fatalf("Execute(feedback): %v", err)
	}
	if !strings.Contains(out, "Revised Plan") || !strings.Contains(out, "security") {
		t.Fatalf("feedback turn should return a revised proposal, got:\n%s", out)
	}
	if !deps.Kernel.PlanAwaitingApproval() {
		t.Fatal("PlanAwaitingApproval = false after revision")
	}

	// Approve turn → the revised graph executes and the merge is returned.
	out, err = deps.Kernel.Execute(context.Background(), "approve")
	if err != nil {
		t.Fatalf("Execute(approve): %v", err)
	}
	if out != "final merged answer" {
		t.Fatalf("approve turn output = %q, want the merged execution result", out)
	}
	if deps.Kernel.PlanAwaitingApproval() {
		t.Error("plan still pending after approval")
	}
	g, ok := deps.Kernel.ConsumeApprovedPlan()
	if !ok || g == nil {
		t.Fatal("ConsumeApprovedPlan: no executed graph retained")
	}
	if len(g.Nodes) != 3 {
		t.Errorf("executed graph has %d nodes, want the revised 3", len(g.Nodes))
	}
	if g2, ok2 := deps.Kernel.ConsumeApprovedPlan(); ok2 || g2 != nil {
		t.Error("ConsumeApprovedPlan should clear after consumption")
	}
}

// TestPlanGateReject verifies reject discards without executing anything.
func TestPlanGateReject(t *testing.T) {
	client := &fakeLLMClient{name: "primary", responses: []string{testPlanJSON}}
	deps := newTestKernel(t, client)
	deps.Kernel.SetPlanControls("always", "light")

	if _, err := deps.Kernel.Execute(context.Background(), "implement the feature and then add tests"); err != nil {
		t.Fatalf("Execute(goal): %v", err)
	}
	callsAfterProposal := client.callCount()

	out, err := deps.Kernel.Execute(context.Background(), "reject")
	if err != nil {
		t.Fatalf("Execute(reject): %v", err)
	}
	if !strings.Contains(out, "discarded") {
		t.Errorf("reject output = %q", out)
	}
	if deps.Kernel.PlanAwaitingApproval() {
		t.Error("plan still pending after reject")
	}
	if got := client.callCount(); got != callsAfterProposal {
		t.Errorf("reject made %d extra LLM calls, want 0", got-callsAfterProposal)
	}
}
