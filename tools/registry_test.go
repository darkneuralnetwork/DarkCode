package tools

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/darkcode/core"
)

func echoEntry(name string) *ToolEntry {
	return &ToolEntry{
		Name:        name,
		Description: "echoes its input",
		Parameters:  MustParseSchema(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			text, _ := args["text"].(string)
			return &ToolResult{Name: name, Success: true, Output: text}
		},
	}
}

func call(id, name, argsJSON string) core.ToolCall {
	return core.ToolCall{ID: id, Type: "function", Function: core.FunctionCall{Name: name, Arguments: argsJSON}}
}

func TestRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(echoEntry("echo"))

	entry, ok := r.Get("echo")
	if !ok || entry.Name != "echo" {
		t.Fatalf("Get(echo) = (%v, %v), want a registered entry", entry, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) should not find an unregistered tool")
	}
}

func TestUnregister(t *testing.T) {
	r := NewRegistry()
	r.Register(echoEntry("echo"))

	if !r.Unregister("echo") {
		t.Fatal("Unregister(echo) should return true for a registered tool")
	}
	if r.Unregister("echo") {
		t.Error("Unregister(echo) a second time should return false")
	}
	if _, ok := r.Get("echo"); ok {
		t.Error("tool should be gone after Unregister")
	}
}

func TestUnregisterBySource(t *testing.T) {
	r := NewRegistry()
	a := echoEntry("a")
	a.Source = "mcp1"
	b := echoEntry("b")
	b.Source = "mcp1"
	c := echoEntry("c")
	c.Source = "builtin"
	r.Register(a)
	r.Register(b)
	r.Register(c)

	removed := r.UnregisterBySource("mcp1")
	if len(removed) != 2 {
		t.Fatalf("UnregisterBySource removed %d tools, want 2", len(removed))
	}
	if _, ok := r.Get("c"); !ok {
		t.Error("tool from a different source should survive UnregisterBySource")
	}
}

func TestDispatchAllUnknownTool(t *testing.T) {
	r := NewRegistry()
	results := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "missing", "{}")}).([]DispatchResult)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Result.Success {
		t.Error("dispatching an unknown tool should fail")
	}
}

func TestDispatchAllInvalidJSON(t *testing.T) {
	r := NewRegistry()
	r.Register(echoEntry("echo"))
	results := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "echo", "{not json")}).([]DispatchResult)
	if results[0].Result.Success {
		t.Error("dispatching with invalid arguments JSON should fail")
	}
}

func TestDispatchAllSuccess(t *testing.T) {
	r := NewRegistry()
	r.Register(echoEntry("echo"))
	results := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "echo", `{"text":"hello"}`)}).([]DispatchResult)
	if !results[0].Result.Success || results[0].Result.Output != "hello" {
		t.Errorf("got %+v, want success with output \"hello\"", results[0].Result)
	}
}

func TestDispatchAllPreservesOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(echoEntry("echo"))
	calls := []core.ToolCall{
		call("1", "echo", `{"text":"a"}`),
		call("2", "echo", `{"text":"b"}`),
		call("3", "echo", `{"text":"c"}`),
	}
	results := r.DispatchAll(context.Background(), calls).([]DispatchResult)
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if results[i].Result.Output != w {
			t.Errorf("result[%d].Output = %q, want %q (order must match input)", i, results[i].Result.Output, w)
		}
	}
}

// --- validateArgs (schema validation added in an earlier session) ---

func TestDispatchValidatesRequiredArguments(t *testing.T) {
	r := NewRegistry()
	r.Register(&ToolEntry{
		Name:       "needs_path",
		Parameters: MustParseSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{Name: "needs_path", Success: true}
		},
	})

	results := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "needs_path", `{}`)}).([]DispatchResult)
	if results[0].Result.Success {
		t.Fatal("dispatch should fail when a required argument is missing")
	}
}

func TestDispatchValidatesArgumentType(t *testing.T) {
	r := NewRegistry()
	r.Register(&ToolEntry{
		Name:       "needs_count",
		Parameters: MustParseSchema(`{"type":"object","properties":{"count":{"type":"integer"}}}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{Name: "needs_count", Success: true}
		},
	})

	results := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "needs_count", `{"count":"not a number"}`)}).([]DispatchResult)
	if results[0].Result.Success {
		t.Fatal("dispatch should fail when an argument's type doesn't match the schema")
	}
}

func TestDispatchValidatesEnum(t *testing.T) {
	r := NewRegistry()
	r.Register(&ToolEntry{
		Name:       "needs_mode",
		Parameters: MustParseSchema(`{"type":"object","properties":{"mode":{"type":"string","enum":["a","b"]}}}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{Name: "needs_mode", Success: true}
		},
	})

	results := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "needs_mode", `{"mode":"c"}`)}).([]DispatchResult)
	if results[0].Result.Success {
		t.Fatal("dispatch should fail when an argument value isn't in the schema's enum")
	}

	okResults := r.DispatchAll(context.Background(), []core.ToolCall{call("1", "needs_mode", `{"mode":"a"}`)}).([]DispatchResult)
	if !okResults[0].Result.Success {
		t.Fatal("dispatch should succeed for a valid enum value")
	}
}

func TestExecuteValidatesArguments(t *testing.T) {
	r := NewRegistry()
	r.Register(&ToolEntry{
		Name:       "needs_path",
		Parameters: MustParseSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			return &ToolResult{Name: "needs_path", Success: true}
		},
	})

	if _, err := r.Execute(context.Background(), "needs_path", map[string]interface{}{}); err == nil {
		t.Fatal("Execute should validate arguments the same way DispatchAll does")
	}
}

// --- timeout handling ---

func TestDispatchRespectsParentContextDeadline(t *testing.T) {
	r := NewRegistry()
	r.Register(&ToolEntry{
		Name: "blocks",
		Handler: func(ctx context.Context, args map[string]interface{}) *ToolResult {
			<-ctx.Done() // hangs until the per-call timeout (derived from the parent) fires
			return &ToolResult{Name: "blocks", Success: false, Error: ctx.Err().Error()}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	results := r.DispatchAll(ctx, []core.ToolCall{call("1", "blocks", "{}")}).([]DispatchResult)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("dispatch took %s, want it to respect the ~50ms parent deadline (not the 120s default)", elapsed)
	}
	if results[0].Result.Success {
		t.Error("a handler that only returns after its context is cancelled should not report success")
	}
}

// --- concurrency ---

func TestDispatchAllConcurrentCalls(t *testing.T) {
	r := NewRegistry()
	r.Register(echoEntry("echo"))

	calls := make([]core.ToolCall, 20)
	for i := range calls {
		calls[i] = call(fmt.Sprintf("%d", i), "echo", fmt.Sprintf(`{"text":"%d"}`, i))
	}
	results := r.DispatchAll(context.Background(), calls).([]DispatchResult)
	if len(results) != 20 {
		t.Fatalf("got %d results, want 20", len(results))
	}
	for i, res := range results {
		want := fmt.Sprintf("%d", i)
		if res.Result.Output != want {
			t.Errorf("result[%d].Output = %q, want %q", i, res.Result.Output, want)
		}
	}
}

func TestRegistryConcurrentRegisterAndDispatch(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			r.Register(echoEntry(fmt.Sprintf("tool%d", i)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = r.List()
			_ = r.Schemas()
			r.DispatchAll(context.Background(), []core.ToolCall{call("1", fmt.Sprintf("tool%d", i), `{"text":"x"}`)})
		}
	}()
	wg.Wait()
}
