package orchestrator

// phase5_e2e_test.go — the end-to-end acceptance test for Phase 5 of the
// context-management unification: with UseCtxEngine on by default, a task
// whose combined recall + project + conversation content would have
// overflowed the old ad hoc caps (boundedChatContext's flat recent-N cutoff,
// getRecallBlock's uncapped skill append) now completes correctly under the
// unified system, and — the part that only works because of the
// UsableBudget fix alongside this phase — a highly-relevant OLD turn that a
// flat recency cutoff would have dropped survives Assemble's ranking.
// Follows finding1_regression_test.go's pattern: a scripted fake client and
// a real Kernel.Execute, no network.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

func TestPhase5_LargeCombinedContextCompletesUnderDefaultConfig(t *testing.T) {
	var lastReq *core.CompletionRequest
	client := &fakeLLMClient{
		name: "fake-primary",
		respFunc: func(idx int, req *core.CompletionRequest) string {
			lastReq = req
			return "Here is the answer, drawing on the widget-building procedure."
		},
		// Left at the default 8000-token window (contextWindow unset) — this
		// only discriminates Assemble's ranking from FitClient's downstream
		// recency-based backstop because AvailableTokens is now
		// ctxfit.UsableBudget(window, 0), not the raw window (see
		// kernel_helpers.go's executeDirectNoTools and infra/ctxfit's
		// UsableBudget doc comment for why that distinction is load-bearing).
	}
	deps := newTestKernel(t, client)

	// Force the no-tools direct path (the one Phase 4 wired Injections into):
	// readOnlyForRequest routes to executeChatReadOnly first, which falls
	// straight back to executeDirectNoTools when agenticLoop is nil.
	deps.Kernel.agenticLoop = nil
	// Disable STM compaction (kernel_execute.go's separate, earlier
	// compression step — see compaction.go) so it doesn't preempt this test
	// by replacing the whole scripted STM with a compressed briefing before
	// executeDirectNoTools/Assemble ever sees it. That step answers "when to
	// shrink stored history"; this test is about Assemble's OWN per-turn
	// ranking of whatever history it's handed, a different question.
	deps.Kernel.cfg.CompressContext = false

	// A single early, highly query-relevant turn. Once the 40 turns below are
	// appended after it, it sits far outside boundedChatContext's old
	// chatContextRecentMax=8 window — the old system drops it unconditionally
	// (it isn't a "[COMPRESSED CONTEXT]" summary, just an ordinary old turn).
	// Assemble (on by default per Phase 5) ranks it against the query and
	// keeps it instead: the one assertion below that actually distinguishes
	// "the unified system worked" from "the old caps would have produced a
	// small-enough request anyway."
	const distinctiveEarlyTurn = "EARLY_MARKER: I already started building the widget, the plan uses module X for the housing"
	deps.Memory.STMAdd(core.Message{Role: core.RoleUser, Content: distinctiveEarlyTurn})

	// A long prior conversation after the marker — far more than
	// boundedChatContext's old chatContextRecentMax=8 cutoff, and (padded)
	// large enough that Assemble's OWN UsableBudget-based trim has to
	// engage, not just FitClient's downstream backstop. Stays under 24 pairs
	// (48 messages) + the marker = 49, deliberately under memory.System's own
	// stmMax=50 ring-buffer cap — that cap evicts oldest-first independently
	// of Assemble, and a fixture that overflowed IT would silently evict the
	// marker before Assemble ever saw it, which is a real, separate context-
	// management mechanism (out of this unification's scope) rather than
	// anything this test means to exercise. Consecutive turns are near-
	// duplicate-ish (~0.5 Jaccard on 3-shingles) but stay under the 0.8 dedup
	// threshold — this fixture exercises TRIMMING, not deduplication; if the
	// topic numbering ever gets more formulaic, check it doesn't start
	// colliding.
	padding := strings.Repeat("filler words to pad this turn out ", 8)
	for i := 0; i < 24; i++ {
		deps.Memory.STMAdd(core.Message{Role: core.RoleUser, Content: fmt.Sprintf("turn %d: tell me something about topic %d. %s", i, i, padding)})
		deps.Memory.STMAdd(core.Message{Role: core.RoleAssistant, Content: fmt.Sprintf("turn %d: here is something about topic %d, in some detail. %s", i, i, padding)})
	}

	// Project plan/workflow — injectProjectContext's own cap (Phase 1-era,
	// maxPlanInjectBytes) still applies independently of Assemble.
	deps.Kernel.SetProjectContext(
		"# Implementation Plan\n"+strings.Repeat("Build the widget subsystem carefully. ", 500),
		"# Workflow\n- [ ] T1: build the widget\n",
	)
	defer deps.Kernel.ClearProjectContext()

	// A recall-worthy skill with enough steps that its rendered block alone
	// would have exceeded the old per-call assumptions — this is what
	// getRecallBlock/Injections carries into executeDirectNoTools now
	// (Phase 4), and getRecallBlock's own maxRecallBlockTotalBytes cap
	// bounds it before it ever reaches Assemble.
	steps := make([]core.SkillStep, 0, 150)
	for i := 0; i < 150; i++ {
		steps = append(steps, core.SkillStep{
			Order:  i + 1,
			Action: "carefully assemble one part of the widget in a fairly verbose way",
			Tool:   "edit_file",
		})
	}
	if err := deps.Memory.ProceduralAdd(&core.Skill{
		Name:        "widget building procedure",
		Description: "builds a widget",
		TriggerCond: "build a widget",
		Steps:       steps,
		SuccessRate: 1.0,
		UseCount:    3,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, restore := deps.Kernel.ApplyRequestOverrides(context.Background(), "", "", "", "readonly", "")
	defer restore()

	out, err := deps.Kernel.Execute(ctx, "build a widget — continue where we left off")
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty answer")
	}
	if lastReq == nil {
		t.Fatal("the fake client never received a request")
	}

	// The discriminating assertion: an old, off-recent-tail turn that is
	// nonetheless highly relevant to the query must have survived. A flat
	// last-8 cutoff (boundedChatContext, the flag-off path) drops this
	// unconditionally regardless of relevance — only relevance-ranked
	// Assemble, given a budget it can actually act on, keeps it. This is
	// what actually proves Phase 5's default flip (plus the UsableBudget fix
	// alongside it) is doing something, as opposed to the old caps alone
	// happening to produce a small-enough request.
	foundMarker := false
	for _, m := range lastReq.Messages {
		if strings.Contains(m.ContentString(), "EARLY_MARKER") {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Error("expected the early, highly-relevant turn to survive Assemble's ranking — a flat recent-N cutoff would have dropped it regardless of relevance")
	}

	// The whole point: this must actually have been bounded. The fake
	// client's ModelInfo().Context is 8000 tokens — assert the assembled
	// request is nowhere near the size of the raw material that went into it
	// (40 turns + a 500-repetition plan + a 150-step skill), proving
	// something actually trimmed it rather than the call succeeding by
	// coincidence.
	total := 0
	for _, m := range lastReq.Messages {
		total += len(m.ContentString())
	}
	const rawInputSize = 48*320 + 500*39 + 150*70 // rough lower bound on the untrimmed material (filler + plan + skill)
	if total >= rawInputSize {
		t.Errorf("assembled request (%d bytes) was not meaningfully smaller than the raw combined input (~%d bytes) — nothing appears to have been trimmed", total, rawInputSize)
	}
	// At ~4 bytes/token, an 8000-token window is ~32000 bytes. Give real
	// margin for estimator differences and message overhead, but a request
	// many times that size would mean the budget wasn't actually respected.
	const maxReasonableBytes = 50000
	if total > maxReasonableBytes {
		t.Errorf("assembled request is %d bytes — expected it bounded well under %d for an 8000-token model window", total, maxReasonableBytes)
	}
}
