package orchestrator

import (
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// TestGetRecallBlock_SurfacesLearnedStrategy closes the gap found in the
// architecture audit: LearningEngine.RecordFeedback/maybeExtractStrategy
// computes a LearnedStrategy (preferred tools/agents for a task type) from
// real outcomes, but nothing ever consulted it — core.LearningStore didn't
// expose SuggestStrategy, so the only two callers of GetAllStrategies were
// display-only (CLI/audit stats). The agent re-derived the same preference
// from scratch on every task of a given type instead of reusing what it had
// already learned, even though the sibling procedural-skill path (recallSkill,
// a few lines below in getRecallBlock) already does exactly this.
func TestGetRecallBlock_SurfacesLearnedStrategy(t *testing.T) {
	deps := newTestKernel(t, nil)

	// Two successful "debug" tasks using the same tool, well above the
	// success-rate and sample-size thresholds maybeExtractStrategy requires.
	for _, goal := range []string{"fix the login bug", "fix the timeout bug"} {
		if err := deps.Memory.Learning().RecordFeedback(core.LearningFeedback{
			TaskGoal:  goal,
			Success:   true,
			ToolsUsed: []string{"read_file", "edit_file"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	block := deps.Kernel.getRecallBlock("fix the signup bug")
	if !strings.Contains(block, "read_file") {
		t.Fatalf("expected recall block to surface the learned strategy's preferred tools, got: %q", block)
	}
}
