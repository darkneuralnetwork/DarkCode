package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/hooks"
)

// The registry has two dispatch paths: DispatchAll (the loop and DAG) and Execute
// (the HTTP/HTP surface). They already duplicate the permission gate, the
// snapshot and the file observation — three chances to fix a bug in one and not
// the other. These tests run every hook assertion against BOTH, so a hook that
// only fires on one path fails here rather than in production on whichever
// surface the reporter happened not to use.

func hookRegistry(t *testing.T, cfg map[string][]hooks.Hook) *Registry {
	t.Helper()
	m, err := hooks.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.SetHooks(m)
	r.Register(&ToolEntry{
		Name:        "write_file",
		Description: "test",
		Parameters:  MustParseSchema(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		Handler: func(context.Context, map[string]interface{}) *ToolResult {
			return &ToolResult{Name: "write_file", Success: true, Output: "wrote"}
		},
	})
	return r
}

// dispatchers runs a tool through each path and returns the result, so one test
// body covers both.
func dispatchers() map[string]func(*Registry, map[string]interface{}) *ToolResult {
	return map[string]func(*Registry, map[string]interface{}) *ToolResult{
		"DispatchAll": func(r *Registry, args map[string]interface{}) *ToolResult {
			raw := r.DispatchAll(context.Background(), []core.ToolCall{{
				ID: "1", Function: core.FunctionCall{Name: "write_file", Arguments: jsonArgs(args)},
			}})
			out, ok := raw.([]DispatchResult)
			if !ok || len(out) == 0 {
				return nil
			}
			return out[0].Result
		},
		"Execute": func(r *Registry, args map[string]interface{}) *ToolResult {
			res, err := r.Execute(context.Background(), "write_file", args)
			if err != nil {
				return &ToolResult{Name: "write_file", Success: false, Error: err.Error()}
			}
			return res
		},
	}
}

func jsonArgs(args map[string]interface{}) string {
	var b strings.Builder
	b.WriteString("{")
	first := true
	for k, v := range args {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + k + `":"` + v.(string) + `"`)
	}
	b.WriteString("}")
	return b.String()
}

// TestPreToolHookBlocksOnBothPaths. A gate one surface honours is not a gate.
func TestPreToolHookBlocksOnBothPaths(t *testing.T) {
	for name, dispatch := range dispatchers() {
		t.Run(name, func(t *testing.T) {
			r := hookRegistry(t, map[string][]hooks.Hook{
				"pre_tool": {{Match: "write_file", Run: `echo "refused by policy" >&2; exit 1`}},
			})
			res := dispatch(r, map[string]interface{}{"path": "x.go"})
			if res == nil {
				t.Fatal("no result")
			}
			if res.Success {
				t.Fatal("the tool ran despite a refusing pre_tool hook")
			}
			if !strings.Contains(res.Error, "refused by policy") {
				t.Errorf("the denial lost the hook's message: %q", res.Error)
			}
		})
	}
}

// TestPostToolHookRunsOnBothPaths, and is told what happened.
func TestPostToolHookRunsOnBothPaths(t *testing.T) {
	for name, dispatch := range dispatchers() {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "seen.txt")
			r := hookRegistry(t, map[string][]hooks.Hook{
				"post_tool": {{Run: `printf '%s|%s|%s' "$DARKCODE_TOOL" "$DARKCODE_FILE" "$DARKCODE_SUCCESS" > ` + out}},
			})
			res := dispatch(r, map[string]interface{}{"path": "x.go"})
			if res == nil || !res.Success {
				t.Fatalf("the tool did not run: %+v", res)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("post_tool never fired on %s: %v", name, err)
			}
			if want := "write_file|x.go|1"; string(got) != want {
				t.Errorf("hook saw %q, want %q", got, want)
			}
		})
	}
}

// TestAPassingPreToolHookLetsTheToolRun — the gate must not be a blanket deny.
func TestAPassingPreToolHookLetsTheToolRun(t *testing.T) {
	for name, dispatch := range dispatchers() {
		t.Run(name, func(t *testing.T) {
			r := hookRegistry(t, map[string][]hooks.Hook{"pre_tool": {{Run: "true"}}})
			if res := dispatch(r, map[string]interface{}{"path": "x.go"}); res == nil || !res.Success {
				t.Errorf("a passing hook blocked the tool: %+v", res)
			}
		})
	}
}

// TestNoHooksConfiguredChangesNothing. The common case must stay free.
func TestNoHooksConfiguredChangesNothing(t *testing.T) {
	for name, dispatch := range dispatchers() {
		t.Run(name, func(t *testing.T) {
			r := hookRegistry(t, nil)
			if res := dispatch(r, map[string]interface{}{"path": "x.go"}); res == nil || !res.Success {
				t.Errorf("an unhooked registry failed: %+v", res)
			}
		})
	}
}
