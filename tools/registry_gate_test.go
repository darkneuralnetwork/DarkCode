package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/permission"
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
