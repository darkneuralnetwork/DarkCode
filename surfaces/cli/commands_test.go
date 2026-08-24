package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestCommandRegistryCoversPhases guards CLI↔GUI parity: every command the
// 5-phase plan surfaces in the CLI must be registered so it appears in the
// searchable /help palette and tab-completion.
func TestCommandRegistryCoversPhases(t *testing.T) {
	have := map[string]cmdInfo{}
	for _, c := range commandRegistry {
		if _, dup := have[c.Name]; dup {
			t.Errorf("duplicate command in registry: %s", c.Name)
		}
		have[c.Name] = c
	}
	// Phase-specific + core commands that must exist.
	required := []string{
		"/help",           // rebuilt palette
		"/always",         // sticky strategy; absorbed the old /chatmode
		"/brain",          // Phase 1: local/cloud/auto
		"/memory-profile", // Phase 1: lean/balanced/max
		"/ingest",         // Phase 3: knowledge ingestion
		"/local", "/model", "/mode", "/safety", "/new",
	}
	for _, name := range required {
		if _, ok := have[name]; !ok {
			t.Errorf("required command %q missing from registry", name)
		}
	}
}

func TestCommandSelectorItemsGroupedAndComplete(t *testing.T) {
	items := commandSelectorItems()
	if len(items) != len(commandRegistry) {
		t.Fatalf("selector items (%d) should match registry (%d)", len(items), len(commandRegistry))
	}
	// Every item must carry a category tag in its description (for grouping +
	// fuzzy search) and a non-empty value to run.
	for _, it := range items {
		if it.Value == "" || it.Title == "" {
			t.Errorf("selector item missing title/value: %+v", it)
		}
		if it.Description == "" {
			t.Errorf("selector item %q missing description", it.Title)
		}
	}
	// Session commands should sort before Observability ones (category order).
	posHelp, posMonitor := -1, -1
	for i, it := range items {
		if it.Value == "/help" {
			posHelp = i
		}
		if it.Value == "/monitor" {
			posMonitor = i
		}
	}
	if posHelp == -1 || posMonitor == -1 || posHelp >= posMonitor {
		t.Errorf("category ordering wrong: /help at %d should precede /monitor at %d", posHelp, posMonitor)
	}
}

// TestEverySpellingIsDispatchable reads the command switch itself, so the alias
// table cannot claim a spelling the console would reject.
//
// The first version of this test compared the alias table against a list
// derived from the alias table, which is self-consistent by construction — it
// passed for an alias the dispatcher had never heard of. Parsing the switch is
// what makes it a check rather than a tautology.
func TestEverySpellingIsDispatchable(t *testing.T) {
	dispatched := dispatchableCommands(t)
	if len(dispatched) < 20 {
		t.Fatalf("only found %d cases in the command switch — the parser is wrong, not the code", len(dispatched))
	}
	for _, s := range CommandSpellings() {
		if !dispatched[s] {
			t.Errorf("%q is offered to the user but the command switch does not accept it", s)
		}
	}
	for name, aliases := range commandAliases {
		for _, a := range aliases {
			if a == name {
				t.Errorf("%q is listed as an alias of itself", a)
			}
		}
	}
}

// TestNoChatModeCommand — /chatmode was a second vocabulary for the question
// the verbs answer, and re-adding it would recreate the ambiguity.
func TestNoChatModeCommand(t *testing.T) {
	if dispatchableCommands(t)["/chatmode"] {
		t.Error("/chatmode is back; /always is the sticky-strategy command")
	}
}

// dispatchableCommands returns every "/..." literal appearing in a case clause
// of the console's slash-command switch.
func dispatchableCommands(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "console_commands.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing the command switch: %v", err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(v, "/") {
				out[v] = true
			}
		}
		return true
	})
	return out
}
