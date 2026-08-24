package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/permission"
)

// spyEntry is a tool whose handler records whether it ran, so tests can assert
// that a denied permission gate actually short-circuits execution.
func spyEntry(name string, ran *bool) *ToolEntry {
	return &ToolEntry{
		Name:        name,
		Description: "records that it ran",
		Parameters:  MustParseSchema(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			*ran = true
			return &ToolResult{Name: name, Success: true, Output: "ran"}
		},
	}
}

// TestExecuteEnforcesGateDeny is the regression guard for the direct-execute
// permission bypass: /api/tools/execute and /api/htp call Registry.Execute,
// which must run the same gate.Check as the ReAct/DAG dispatch path. A denied
// call must not invoke the handler.
func TestExecuteEnforcesGateDeny(t *testing.T) {
	r := NewRegistry()
	ran := false
	r.Register(spyEntry("spy", &ran))

	gate := permission.NewGate(permission.LevelStrict)
	gate.SetApprover(permission.AutoDeny())
	r.SetPermissionGate(gate)

	res, err := r.Execute(context.Background(), "spy", map[string]interface{}{"text": "hi"})
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if ran {
		t.Fatal("handler ran despite the permission gate denying the call")
	}
	if res == nil || res.Success {
		t.Fatalf("expected an unsuccessful result, got %+v", res)
	}
	if !strings.Contains(res.Error, "permission denied") {
		t.Fatalf("expected a permission-denied error, got %q", res.Error)
	}
}

// TestExecuteEnforcesGateAllow confirms the gate does not block approved calls:
// with an auto-approver the handler runs and the result is successful.
func TestExecuteEnforcesGateAllow(t *testing.T) {
	r := NewRegistry()
	ran := false
	r.Register(spyEntry("spy", &ran))

	gate := permission.NewGate(permission.LevelStrict)
	gate.SetApprover(permission.AutoApprover())
	r.SetPermissionGate(gate)

	res, err := r.Execute(context.Background(), "spy", map[string]interface{}{"text": "hi"})
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if !ran {
		t.Fatal("handler did not run despite the permission gate approving the call")
	}
	if res == nil || !res.Success {
		t.Fatalf("expected a successful result, got %+v", res)
	}
}

// TestEveryDispatchSurfaceGates. There are three ways a tool runs: Execute for
// the HTTP and HTP surfaces, and DispatchAll for the ReAct loop, the DAG, and
// sub-agents. A deny rule that held on one and not another would be a rule the
// user believes is in force while an agent routes around it — sub-agents in
// particular dispatch their own calls.
//
// This asserts on behaviour rather than on the call graph, so a fourth surface
// added later is covered only if it actually gates.
func TestEveryDispatchSurfaceGates(t *testing.T) {
	gate := permission.NewGate(permission.LevelRelaxed)
	// A deny rule beats the relaxed level, which is the point of deny rules.
	gate.SetDenyRules([]string{"danger"})

	surfaces := map[string]func(r *Registry) bool{
		"Execute": func(r *Registry) bool {
			res, _ := r.Execute(context.Background(), "danger", map[string]interface{}{"text": "x"})
			return res != nil && !res.Success
		},
		"DispatchAll": func(r *Registry) bool {
			out := r.DispatchAll(context.Background(), []core.ToolCall{{
				ID: "c1", Function: core.FunctionCall{Name: "danger", Arguments: `{"text":"x"}`},
			}})
			results, ok := out.([]DispatchResult)
			return ok && len(results) == 1 && results[0].Result != nil && !results[0].Result.Success
		},
	}

	for name, call := range surfaces {
		t.Run(name, func(t *testing.T) {
			ran := false
			r := NewRegistry()
			r.Register(spyEntry("danger", &ran))
			r.SetPermissionGate(gate)

			if refused := call(r); !refused {
				t.Errorf("%s allowed a call a deny rule forbids", name)
			}
			if ran {
				t.Errorf("%s invoked the handler despite the deny rule", name)
			}
		})
	}
}

// TestDenyRuleBeatsAnAllowedSession. A rule is meant to hold "no matter how the
// session was configured or what was approved earlier"; a cached session
// approval must not step around it.
func TestDenyRuleBeatsAnAllowedSession(t *testing.T) {
	gate := permission.NewGate(permission.LevelStrict)
	gate.SetApprover(func(req permission.ApprovalRequest) permission.Verdict {
		return permission.Verdict{Decision: permission.DecisionAllowSession}
	})

	ran := false
	r := NewRegistry()
	r.Register(spyEntry("danger", &ran))
	r.SetPermissionGate(gate)

	// Approve it for the session first.
	if res, _ := r.Execute(context.Background(), "danger", map[string]interface{}{"text": "ok"}); res == nil || !res.Success {
		t.Fatal("the first call should have been approved for the session")
	}
	ran = false

	// Now add the rule. The earlier session approval must not survive it.
	gate.SetDenyRules([]string{"danger"})
	res, _ := r.Execute(context.Background(), "danger", map[string]interface{}{"text": "ok"})
	if res != nil && res.Success {
		t.Error("a session approval let a call through a deny rule")
	}
	if ran {
		t.Error("the handler ran despite the deny rule")
	}
}
