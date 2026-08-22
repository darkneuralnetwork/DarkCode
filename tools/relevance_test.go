package tools

import (
	"encoding/json"
	"testing"

	"github.com/darkcode/core"
	"github.com/darkcode/security"
)

func fullSchemas(t *testing.T) []core.ToolSchema {
	t.Helper()
	reg := NewRegistry()
	RegisterBuiltinTools(reg, nil, nil, security.NewSandbox(nil))
	all, _ := reg.LLMSchemas().([]core.ToolSchema)
	return all
}

func has(s []core.ToolSchema, name string) bool {
	for _, x := range s {
		if x.Function.Name == name {
			return true
		}
	}
	return false
}

// TestRelevanceKeepsTheCoreToolset. Withholding a tool a coding agent reaches
// for without being prompted trades a certain saving for an uncertain failure.
func TestRelevanceKeepsTheCoreToolset(t *testing.T) {
	all := fullSchemas(t)
	sub := RelevantSchemas("rename the Greet function to Welcome", all, nil)
	for _, must := range []string{"read_file", "write_file", "list_dir", "search_files", "terminal"} {
		if !has(sub, must) {
			t.Errorf("%s was withheld from an ordinary coding task", must)
		}
	}
}

// TestRelevanceDropsUnrelatedSpecialists — the actual saving.
func TestRelevanceDropsUnrelatedSpecialists(t *testing.T) {
	all := fullSchemas(t)
	sub := RelevantSchemas("rename the Greet function to Welcome", all, nil)
	for _, gone := range []string{"pdf", "image", "browser_subagent", "monitoring"} {
		if has(sub, gone) {
			t.Errorf("%s was sent to a task that cannot use it", gone)
		}
	}
	full, _ := json.Marshal(all)
	small, _ := json.Marshal(sub)
	if len(small) >= len(full) {
		t.Fatalf("no saving: %d vs %d bytes", len(small), len(full))
	}
	if saving := 100 - (len(small) * 100 / len(full)); saving < 40 {
		t.Errorf("saving is only %d%%; the filter is not earning its complexity", saving)
	}
}

// TestRelevanceIncludesMentionedDomains — a goal that names the domain gets
// the tool, or the filter has broken the feature to save bytes.
func TestRelevanceIncludesMentionedDomains(t *testing.T) {
	all := fullSchemas(t)
	cases := map[string]string{
		"extract the tables from this pdf report": "pdf",
		"commit the changes and push":             "git",
		"take a screenshot of the page":           "image",
		"check cpu and memory usage":              "monitoring",
	}
	for goal, want := range cases {
		if !has(RelevantSchemas(goal, all, nil), want) {
			t.Errorf("goal %q did not get %s", goal, want)
		}
	}
}

// TestUnlockedToolsAreReoffered. This is what makes a wrong guess cost one turn
// instead of the task: a tool the model asked for anyway comes back next turn.
func TestUnlockedToolsAreReoffered(t *testing.T) {
	all := fullSchemas(t)
	goal := "rename the Greet function to Welcome"
	if has(RelevantSchemas(goal, all, nil), "pdf") {
		t.Fatal("fixture assumption broken: pdf should start filtered out")
	}
	sub := RelevantSchemas(goal, all, map[string]bool{"pdf": true})
	if !has(sub, "pdf") {
		t.Error("a tool the model asked for was not re-offered")
	}
}

// TestUnclassifiedToolsAreOffered. An unlisted tool means nobody has
// classified it yet; silently withholding one would make adding a tool a trap.
func TestUnclassifiedToolsAreOffered(t *testing.T) {
	all := []core.ToolSchema{{Type: "function", Function: core.FunctionDef{Name: "brand_new_tool"}}}
	if !has(RelevantSchemas("do something unrelated", all, nil), "brand_new_tool") {
		t.Error("an unclassified tool was withheld")
	}
}
