package orchestrator

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
	"github.com/darkcode/recall"
)

// skill_extractor.go — the learning loop.
//
// A finished task leaves behind evidence of what worked: which roles ran, in
// what order, and which tools they used. extractSkill turns that into a
// procedural memory entry, and recallSkill finds it again when a similar goal
// arrives. Without the recall half, extraction is just write-only bookkeeping;
// the two together are what make the second attempt at a kind of task cheaper
// than the first.

// extractSkill promotes a successful multi-step task into a reusable
// procedural Skill. successCount and toolsUsed are computed once by the
// caller (recordLearningAndAudit) from the same `results` it also feeds to
// the Learning Engine, rather than being re-derived here from a second pass
// over results — see recordLearningAndAudit's doc comment.
func (k *Kernel) extractSkill(goal string, results []*core.SubAgentResult, successCount int, toolsUsed []string, minSuccess int) {
	if successCount < minSuccess || len(toolsUsed) == 0 {
		return // not complex enough, or no tools were used
	}

	// Steps record the actual execution: each successful sub-task in order,
	// tagged with its role. A generic "decompose the goal" placeholder would
	// teach the next run nothing it doesn't already do.
	var steps []core.SkillStep
	for _, r := range results {
		if !r.Success {
			continue
		}
		steps = append(steps, core.SkillStep{
			Order:  len(steps) + 1,
			Action: r.Goal,
			Tool:   string(r.Role),
		})
	}
	if len(steps) == 0 {
		return
	}

	skillName := generateSkillName(goal)
	if existing, ok := k.memory.ProceduralGet(skillName); ok {
		// Reinforce: the more often a skill's shape recurs, the more it is
		// worth trusting. Success rate is a running mean over uses.
		existing.UseCount++
		now := time.Now()
		existing.LastUsed = &now
		existing.SuccessRate = (existing.SuccessRate*float64(existing.UseCount-1) + 1.0) / float64(existing.UseCount)
		if len(steps) > len(existing.Steps) {
			existing.Steps = steps // keep the richer trace
		}
		_ = k.remember(recall.Procedure{Skill: existing})
		return
	}

	skill := &core.Skill{
		Name:        skillName,
		Description: goal,
		Steps:       steps,
		TriggerCond: goal,
		CreatedAt:   time.Now(),
		UseCount:    1,
		SuccessRate: 1.0,
		Metadata:    map[string]string{"tools": strings.Join(toolsUsed, ",")},
	}
	_ = k.remember(recall.Procedure{Skill: skill})
	k.log("improve", fmt.Sprintf("Extracted skill: %s (%d steps)", skillName, len(steps)))
}

// skillMinScore is the keyword-overlap fraction a stored skill must reach
// before it is offered as precedent. Below it the "relevant experience" is
// noise, and injecting noise costs tokens and misleads the planner.
const skillMinScore = 0.34

// recallSkill returns the most relevant past procedure for a goal, rendered
// for prompt injection, or "" when nothing is close enough.
//
// This is the half of the learning loop that pays for the other half: the
// agent gets to see how it solved this shape of problem before, instead of
// rediscovering it.
func (k *Kernel) recallSkill(goal string) string {
	if k.memory == nil {
		return ""
	}
	skills := k.memory.ProceduralAll()
	if len(skills) == 0 {
		return ""
	}

	want := keywordSet(goal)
	if len(want) == 0 {
		return ""
	}

	type scored struct {
		skill *core.Skill
		score float64
	}
	var best []scored
	for _, s := range skills {
		overlap := 0
		for w := range keywordSet(s.TriggerCond + " " + s.Description + " " + s.Name) {
			if want[w] {
				overlap++
			}
		}
		score := float64(overlap) / float64(len(want))
		if score >= skillMinScore {
			// A procedure that has worked repeatedly outranks a one-off with
			// the same word overlap.
			best = append(best, scored{s, score * (0.5 + 0.5*s.SuccessRate)})
		}
	}
	if len(best) == 0 {
		return ""
	}
	sort.Slice(best, func(i, j int) bool { return best[i].score > best[j].score })

	s := best[0].skill
	var b strings.Builder

	// Two kinds of skill reach this point, and they carry different authority.
	// A learned one has a success rate measured from real runs on this machine.
	// An imported one was written down by somebody and has never been tried
	// here, so "worked 0 time(s), 0% success" would be both untrue and
	// actively harmful — it teaches the model to distrust good guidance for
	// the sole reason that it is new. Say which kind it is.
	if s.Metadata["origin"] == memory.OriginImported {
		fmt.Fprintf(&b, "## Relevant Written Procedure — %s\n", s.Name)
		b.WriteString("_Authored guidance, not measured here")
		if s.UseCount > 0 {
			fmt.Fprintf(&b, "; applied %d time(s) since, %.0f%% success", s.UseCount, s.SuccessRate*100)
		}
		b.WriteString(". Adapt it; don't follow it blindly._\n")
	} else {
		fmt.Fprintf(&b, "## Relevant Past Procedure — %s\n", s.Name)
		fmt.Fprintf(&b, "_Worked %d time(s), %.0f%% success. Adapt it; don't follow it blindly._\n",
			s.UseCount, s.SuccessRate*100)
	}
	for _, step := range s.Steps {
		if step.Tool != "" {
			fmt.Fprintf(&b, "%d. [%s] %s\n", step.Order, step.Tool, step.Action)
		} else {
			fmt.Fprintf(&b, "%d. %s\n", step.Order, step.Action)
		}
	}
	k.log("memory", fmt.Sprintf("Recalled skill %q as precedent", s.Name))

	// Reusing a procedure is evidence about it; record the use so an
	// unhelpful skill's success rate can fall on the next outcome.
	now := time.Now()
	s.LastUsed = &now
	_ = k.remember(recall.Procedure{Skill: s})
	return b.String()
}

// skillStopWords are too common to carry meaning in a goal.
var skillStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "to": true,
	"for": true, "of": true, "in": true, "on": true, "with": true, "is": true,
	"it": true, "this": true, "that": true, "please": true, "can": true,
	"you": true, "i": true, "we": true, "my": true, "me": true, "be": true,
	"do": true, "make": true, "into": true, "from": true, "by": true, "at": true,
}

// keywordSet reduces text to its meaningful lowercase words.
func keywordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) > 2 && !skillStopWords[w] {
			out[w] = true
		}
	}
	return out
}

// compressionKeepRecent is how many of the most recent messages are kept
// verbatim after compression (for conversational continuity: the compressed
// briefing captures the gist, the recent tail preserves the active context).
const compressionKeepRecent = 4
