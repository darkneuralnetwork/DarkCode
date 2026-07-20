package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/llm"
)

func roEntry(name string, readOnly bool, ran *bool) *ToolEntry {
	return &ToolEntry{
		Name:        name,
		Description: name,
		Parameters:  MustParseSchema(`{"type":"object","properties":{}}`),
		ReadOnly:    readOnly,
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			*ran = true
			return &ToolResult{Name: name, Success: true, Output: "ran"}
		},
	}
}

// TestReadOnlyContextBlocksMutatingTool verifies the Chat-mode guard: under a
// read-only context, a mutating tool is refused and never runs, while a
// read-only tool runs normally.
func TestReadOnlyContextBlocksMutatingTool(t *testing.T) {
	r := NewRegistry()
	writeRan, readRan := false, false
	r.Register(roEntry("write_thing", false, &writeRan))
	r.Register(roEntry("read_thing", true, &readRan))

	roCtx := context.WithValue(context.Background(), core.ReadOnlyToolsKey, true)

	// Mutating tool refused.
	res, err := r.Execute(roCtx, "write_thing", map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writeRan {
		t.Fatal("mutating tool ran under read-only context")
	}
	if res == nil || res.Success || !strings.Contains(res.Error, "read-only") {
		t.Fatalf("expected a read-only refusal, got %+v", res)
	}

	// Read-only tool allowed.
	res, err = r.Execute(roCtx, "read_thing", map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !readRan || res == nil || !res.Success {
		t.Fatalf("read-only tool should run, got ran=%v res=%+v", readRan, res)
	}

	// Without the read-only context, the mutating tool runs.
	writeRan = false
	if _, err := r.Execute(context.Background(), "write_thing", map[string]interface{}{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !writeRan {
		t.Fatal("mutating tool should run without the read-only context")
	}
}

// TestLLMSchemasReadOnlyExcludesMutating verifies Chat is only ever offered
// read-only tool schemas.
func TestLLMSchemasReadOnlyExcludesMutating(t *testing.T) {
	r := NewRegistry()
	var x bool
	r.Register(roEntry("read_thing", true, &x))
	r.Register(roEntry("write_thing", false, &x))

	ro := r.LLMSchemasReadOnly().([]llm.ToolSchema)
	names := map[string]bool{}
	for _, s := range ro {
		names[s.Function.Name] = true
	}
	if !names["read_thing"] {
		t.Error("read-only schema set should include read_thing")
	}
	if names["write_thing"] {
		t.Error("read-only schema set must NOT include write_thing")
	}

	all := r.LLMSchemas().([]llm.ToolSchema)
	if len(all) <= len(ro) {
		t.Errorf("full schema set (%d) should be larger than read-only (%d)", len(all), len(ro))
	}
}
