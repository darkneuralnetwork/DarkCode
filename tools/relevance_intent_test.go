package tools

import (
	"encoding/json"
	"testing"

	"github.com/darkcode/core"
)

func schemaSet(names ...string) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(names))
	for _, n := range names {
		s := core.ToolSchema{Type: "function"}
		s.Function.Name = n
		out = append(out, s)
	}
	return out
}

func offered(goal string, names ...string) map[string]bool {
	got := map[string]bool{}
	for _, s := range RelevantSchemas(goal, schemaSet(names...), nil) {
		got[s.Function.Name] = true
	}
	return got
}

// TestDifferentiatingToolsAreAlwaysOffered is the regression guard for DC-15.
//
// The filter used to gate graph_query, lsp and debug behind words the USER had
// to type. "fix the failing test" mentions no graph, no symbol and no
// breakpoint, so the three tools that most distinguish this agent from a plain
// ReAct loop were withheld from exactly the task they exist for. The
// unlock-on-request escape could not rescue it either: a model cannot ask for a
// tool it was never shown.
func TestDifferentiatingToolsAreAlwaysOffered(t *testing.T) {
	realistic := []string{
		"fix the failing test",
		"why does the build break",
		"clean up this package",
		"make the login flow work again",
	}
	must := []string{"graph_query", "lsp", "debug", "git", "web_search", "todo", "research"}

	for _, goal := range realistic {
		got := offered(goal, append([]string{"read_file", "terminal"}, must...)...)
		for _, tool := range must {
			if !got[tool] {
				t.Errorf("goal %q was not offered %q — the agent cannot use a tool it is never shown", goal, tool)
			}
		}
	}
}

// TestNarrowToolsStillGated — the saving has to survive. These four are 37% of
// the schema payload and a coding task genuinely never needs them unsolicited.
func TestNarrowToolsStillGated(t *testing.T) {
	narrow := []string{"pdf", "image", "browser_subagent", "monitoring"}
	all := append([]string{"read_file", "terminal", "graph_query"}, narrow...)

	got := offered("rename the Handler type across the package", all...)
	for _, tool := range narrow {
		if got[tool] {
			t.Errorf("a Go rename task was offered %q; the narrow tools must stay gated or the filter saves nothing", tool)
		}
	}
	if !got["graph_query"] {
		t.Error("a rename task must be offered graph_query")
	}

	// …and they appear when the goal actually calls for them.
	if !offered("extract the tables from this pdf", all...)["pdf"] {
		t.Error("a PDF task was not offered the pdf tool")
	}
	if !offered("check cpu and disk usage", all...)["monitoring"] {
		t.Error("a resource-usage task was not offered monitoring")
	}
}

// TestAskedForToolIsUnlocked keeps the escape hatch working for the narrow set.
func TestAskedForToolIsUnlocked(t *testing.T) {
	all := schemaSet("read_file", "pdf")
	got := map[string]bool{}
	for _, s := range RelevantSchemas("summarise the attached file", all, map[string]bool{"pdf": true}) {
		got[s.Function.Name] = true
	}
	if !got["pdf"] {
		t.Error("a tool the model already asked for must stay offered for the rest of the run")
	}
}

// TestFilterStillSavesMeaningfully — the point of the filter is tokens, so
// prove it still removes a worthwhile share on a typical coding goal.
func TestFilterStillSavesMeaningfully(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltinTools(r, nil, nil, nil)
	all, _ := r.LLMSchemas().([]core.ToolSchema)

	size := func(ss []core.ToolSchema) int {
		n := 0
		for _, s := range ss {
			b, _ := json.Marshal(s)
			n += len(b)
		}
		return n
	}

	full := size(all)
	kept := size(RelevantSchemas("fix the failing test in the router package", all, nil))
	saved := 100 * float64(full-kept) / float64(full)

	if saved < 20 {
		t.Errorf("filter saved only %.1f%% of %d bytes; the narrow tools are ~37%% of the payload, so something stopped being gated", saved, full)
	}
	t.Logf("typical coding goal: %d → %d bytes (%.1f%% saved)", full, kept, saved)
}
