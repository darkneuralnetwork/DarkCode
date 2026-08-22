package orchestrator

import (
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
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

// TestGetRecallBlock_CapsCombinedOutput is the regression test for Phase 4 of
// the context-management unification: FormatRecall caps its own portion at
// memory.maxRecallBlockLen (2000 bytes), but recallSkill's rendered steps and
// recallStrategy's summary used to be appended after it with no combined
// cap — a skill with many verbose steps could push the total arbitrarily
// large. Build a skill whose keywords match the goal (clears skillMinScore)
// with enough steps that its rendered block alone exceeds
// maxRecallBlockTotalBytes, then confirm the combined output is capped.
func TestGetRecallBlock_CapsCombinedOutput(t *testing.T) {
	deps := newTestKernel(t, nil)

	steps := make([]core.SkillStep, 0, 200)
	for i := 0; i < 200; i++ {
		steps = append(steps, core.SkillStep{
			Order:  i + 1,
			Action: strings.Repeat("do a verbose, wordy thing in this step ", 5),
			Tool:   "edit_file",
		})
	}
	skill := &core.Skill{
		Name:        "widget building procedure",
		Description: "builds a widget",
		TriggerCond: "build a widget",
		Steps:       steps,
		SuccessRate: 1.0,
		UseCount:    5,
	}
	if err := deps.Memory.ProceduralAdd(skill); err != nil {
		t.Fatal(err)
	}

	block := deps.Kernel.getRecallBlock("build a widget")
	if len(block) > maxRecallBlockTotalBytes+64 { // small slack for TruncateMid's marker
		t.Fatalf("getRecallBlock output not capped: got %d bytes, want <= ~%d", len(block), maxRecallBlockTotalBytes)
	}
	if block == "" {
		t.Fatal("expected a non-empty recall block (the skill should have matched)")
	}
}
