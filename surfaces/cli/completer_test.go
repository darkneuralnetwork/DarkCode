package cli

import (
	"reflect"
	"testing"

	"github.com/darkcode/infra/config"
)

// TestBuildCompleterCoversEveryCanonicalCommand diffs commandRegistry's
// canonical command names (commands.go — the source of truth /help's
// palette already reads from) against buildCompleter()'s top-level PcItem
// set. Before this, 11 of 44 registered commands (/rollback, /session,
// /project, /health, /evolution, /audit, /learning, /lock-tests, /monitor,
// /plan, /workflow) were missing from tab-completion entirely — they only
// existed if a user found them via /help. This test makes that gap
// structurally impossible to reopen: adding a command to commandRegistry
// without also adding it to buildCompleter() now fails CI.
func TestBuildCompleterCoversEveryCanonicalCommand(t *testing.T) {
	c := &Console{cfg: &config.Config{}}
	completer := c.buildCompleter()

	offered := make(map[string]bool, len(completer.Children))
	for _, child := range completer.Children {
		offered[string(child.GetName())] = true
	}

	for _, cmd := range commandRegistry {
		if !offered[cmd.Name+" "] {
			t.Errorf("commandRegistry has %q but buildCompleter() does not offer it for tab-completion", cmd.Name)
		}
	}
}

func TestCompleteModelNames_PrimaryAndConfiguredModels(t *testing.T) {
	c := &Console{cfg: &config.Config{
		Model: "gpt-4o",
		Models: map[string]config.ModelConfig{
			"claude-sonnet": {Model: "claude-sonnet-5"},
			"local-llama":   {Model: "llama"},
		},
	}}

	got := c.completeModelNames("")
	want := []string{"claude-sonnet", "gpt-4o", "local-llama"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("completeModelNames() = %v, want %v", got, want)
	}
}

func TestCompleteModelNames_NoDuplicateWhenPrimaryAlsoInModelsMap(t *testing.T) {
	c := &Console{cfg: &config.Config{
		Model: "gpt-4o",
		Models: map[string]config.ModelConfig{
			"gpt-4o": {Model: "gpt-4o"},
		},
	}}

	got := c.completeModelNames("")
	want := []string{"gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("completeModelNames() = %v, want %v (no duplicate entry)", got, want)
	}
}

func TestCompleteModelNames_NoPrimaryConfigured(t *testing.T) {
	c := &Console{cfg: &config.Config{
		Models: map[string]config.ModelConfig{
			"local-llama": {Model: "llama"},
		},
	}}

	got := c.completeModelNames("")
	want := []string{"local-llama"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("completeModelNames() = %v, want %v", got, want)
	}
}
