package cli

import (
	"reflect"
	"testing"

	"github.com/darkcode/config"
)

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
