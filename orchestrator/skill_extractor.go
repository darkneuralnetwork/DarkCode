package orchestrator

import (
	"fmt"
	"time"

	"github.com/darkcode/core"
)

// extractSkill promotes a successful multi-step task into a reusable
// procedural Skill. successCount and toolsUsed are computed once by the
// caller (recordLearningAndAudit) from the same `results` it also feeds to
// the Learning Engine, rather than being re-derived here from a second pass
// over results — see recordLearningAndAudit's doc comment.
func (k *Kernel) extractSkill(goal string, results []*core.SubAgentResult, successCount int, toolsUsed []string, minSuccess int) {
	if successCount < minSuccess || len(toolsUsed) == 0 {
		return // not complex enough, or no tools were used
	}

	// Build skill steps from the task pattern
	var steps []core.SkillStep
	steps = append(steps, core.SkillStep{Order: 1, Action: "Decompose the goal into sub-tasks"})
	for i, r := range results {
		if r.Success {
			steps = append(steps, core.SkillStep{
				Order:  i + 2,
				Action: fmt.Sprintf("%s: %s", r.Role, r.Goal),
			})
		}
	}
	steps = append(steps, core.SkillStep{Order: len(results) + 2, Action: "Merge results and verify"})

	// Check if a similar skill already exists
	skillName := generateSkillName(goal)
	if existing, ok := k.memory.ProceduralGet(skillName); ok {
		// Update existing skill
		existing.UseCount++
		now := time.Now()
		existing.LastUsed = &now
		existing.SuccessRate = (existing.SuccessRate*float64(existing.UseCount-1) + 1.0) / float64(existing.UseCount)
		_ = k.memory.ProceduralAdd(existing)
		return
	}

	// Create new skill
	skill := &core.Skill{
		Name:        skillName,
		Description: fmt.Sprintf("Auto-extracted skill for: %s", goal),
		Steps:       steps,
		CreatedAt:   time.Now(),
		UseCount:    1,
		SuccessRate: 1.0,
	}

	_ = k.memory.ProceduralAdd(skill)
	k.log("improve", fmt.Sprintf("Extracted skill: %s", skillName))
}

// compressionMinHistory is the minimum STM length before the compressor is
// invoked. Below this, the conversation is short enough that an LLM
// summarization call costs more than it saves.
const compressionMinHistory = 8

// compressionMinGrowth is the minimum number of new messages since the last
// compression before we compress again. Prevents re-compressing the same
// window twice when two requests land while STM is between thresholds.
const compressionMinGrowth = 4

// compressionKeepRecent is how many of the most recent messages are kept
// verbatim after compression (for conversational continuity: the compressed
// briefing captures the gist, the recent tail preserves the active context).
const compressionKeepRecent = 4
