package orchestrator

import (
	"context"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/tools/tools"
)

func TestUsedMutatingToolNoCalls(t *testing.T) {
	deps := newTestKernel(t, nil)
	if deps.Kernel.usedMutatingTool(nil) {
		t.Fatal("no tool calls at all should never count as mutating")
	}
}

func TestUsedMutatingToolUnknownNameDefaultsToMutating(t *testing.T) {
	deps := newTestKernel(t, nil)
	calls := []core.ToolCall{{Function: core.FunctionCall{Name: "not_a_registered_tool"}}}
	if !deps.Kernel.usedMutatingTool(calls) {
		t.Fatal("an unrecognized tool name should default to mutating (verify, don't skip)")
	}
}

func TestUsedMutatingToolReadOnlyDoesNotForceVerification(t *testing.T) {
	deps := newTestKernel(t, nil)
	deps.Registry.Register(&tools.ToolEntry{
		Name:     "look",
		ReadOnly: true,
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			return &tools.ToolResult{Name: "look", Success: true}
		},
	})
	calls := []core.ToolCall{{Function: core.FunctionCall{Name: "look"}}}
	if deps.Kernel.usedMutatingTool(calls) {
		t.Fatal("a read-only tool call should not force verification")
	}
}

func TestUsedMutatingToolWriteForcesVerification(t *testing.T) {
	deps := newTestKernel(t, nil)
	deps.Registry.Register(&tools.ToolEntry{
		Name:     "poke",
		ReadOnly: false,
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			return &tools.ToolResult{Name: "poke", Success: true}
		},
	})
	calls := []core.ToolCall{{Function: core.FunctionCall{Name: "poke"}}}
	if !deps.Kernel.usedMutatingTool(calls) {
		t.Fatal("a mutating tool call must force verification")
	}
}

func TestUsedMutatingToolMixedCallsIsMutating(t *testing.T) {
	deps := newTestKernel(t, nil)
	deps.Registry.Register(&tools.ToolEntry{
		Name:     "look",
		ReadOnly: true,
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			return &tools.ToolResult{Name: "look", Success: true}
		},
	})
	deps.Registry.Register(&tools.ToolEntry{
		Name:     "poke",
		ReadOnly: false,
		Handler: func(ctx context.Context, args map[string]interface{}) *tools.ToolResult {
			return &tools.ToolResult{Name: "poke", Success: true}
		},
	})
	calls := []core.ToolCall{
		{Function: core.FunctionCall{Name: "look"}},
		{Function: core.FunctionCall{Name: "poke"}},
	}
	if !deps.Kernel.usedMutatingTool(calls) {
		t.Fatal("one mutating call among several must still force verification")
	}
}
