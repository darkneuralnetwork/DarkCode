package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/tools/tools"
)

func scopeTestRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(&tools.ToolEntry{
		Name: "read_file", Description: "read a file", ReadOnly: true,
		Parameters: tools.MustParseSchema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, a map[string]interface{}) *tools.ToolResult {
			return &tools.ToolResult{Name: "read_file", Success: true, Output: "contents"}
		},
	})
	reg.Register(&tools.ToolEntry{
		Name: "terminal", Description: "run a shell command", ReadOnly: false,
		Parameters: tools.MustParseSchema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, a map[string]interface{}) *tools.ToolResult {
			return &tools.ToolResult{Name: "terminal", Success: true, Output: "RAN THE COMMAND"}
		},
	})
	return reg
}

// TestResearchAgentIsNotOfferedWriteTools. A research agent's whole job is to
// read untrusted material and summarise it. Handing it a shell is the shortest
// prompt-injection path in the system.
func TestResearchAgentIsNotOfferedWriteTools(t *testing.T) {
	reg := scopeTestRegistry(t)

	for _, role := range []core.AgentRole{
		core.RoleResearch, core.RoleCritic, core.RoleQA, core.RoleSecurity, core.RolePlanner,
	} {
		t.Run(string(role), func(t *testing.T) {
			schemas := schemasFor(reg, core.SubAgentConfig{Role: role})
			for _, s := range schemas {
				if s.Function.Name == "terminal" {
					t.Errorf("%s was offered the terminal tool", role)
				}
			}
			if len(schemas) == 0 {
				t.Errorf("%s was offered no tools at all; it still needs to read", role)
			}
		})
	}
}

// TestWorkerKeepsFullToolset — scoping must not disarm the roles whose job is
// to change things.
func TestWorkerKeepsFullToolset(t *testing.T) {
	reg := scopeTestRegistry(t)
	for _, role := range []core.AgentRole{core.RoleWorker, core.RoleOps} {
		schemas := schemasFor(reg, core.SubAgentConfig{Role: role})
		found := false
		for _, s := range schemas {
			if s.Function.Name == "terminal" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s lost the terminal tool it needs to do its job", role)
		}
	}
}

// TestDispatchRefusesWritesForScopedRole is the boundary rather than the
// ergonomics. Not offering a tool is a hint; refusing to run it is the actual
// protection, and it is what stops a model that invents a tool name — or is
// talked into one by a page it was asked to summarise.
func TestDispatchRefusesWritesForScopedRole(t *testing.T) {
	reg := scopeTestRegistry(t)

	ctx := context.WithValue(context.Background(), core.ReadOnlyToolsKey, true)
	ctx = context.WithValue(ctx, core.ReadOnlyReasonKey, "the research role has no write authority")

	res, err := reg.Execute(ctx, "terminal", map[string]interface{}{"command": "rm -rf /"})
	if err == nil && res != nil && res.Success {
		t.Fatal("a scoped agent's terminal call was executed")
	}
	if res != nil && strings.Contains(res.Output, "RAN THE COMMAND") {
		t.Fatal("the handler ran despite the scope")
	}
	if res != nil && !strings.Contains(res.Error, "write authority") {
		t.Errorf("refusal should explain the real reason, got: %q", res.Error)
	}
}

// TestExplicitToolListNarrowsButCannotWiden. A caller may say a worker only
// needs two tools; a caller must not be able to hand a research agent a shell.
func TestExplicitToolListNarrowsButCannotWiden(t *testing.T) {
	reg := scopeTestRegistry(t)

	narrowed := schemasFor(reg, core.SubAgentConfig{
		Role: core.RoleWorker, Tools: []string{"read_file"},
	})
	if len(narrowed) != 1 || narrowed[0].Function.Name != "read_file" {
		t.Errorf("explicit list did not narrow: %v", names(narrowed))
	}

	widened := schemasFor(reg, core.SubAgentConfig{
		Role: core.RoleResearch, Tools: []string{"terminal", "read_file"},
	})
	for _, s := range widened {
		if s.Function.Name == "terminal" {
			t.Error("an explicit list widened a read-only role's authority")
		}
	}
}

func names(s []core.ToolSchema) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Function.Name)
	}
	return out
}
