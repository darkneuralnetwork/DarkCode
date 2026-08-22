# Recon: Orchestrator / Kernel subsystem (Tier 1)

> **Note:** this is a point-in-time subsystem pass, one of five inputs consolidated into `../../DARKCODE_RECON_REPORT.md`. Several items marked UNKNOWN or unverified here (notably: the SSRF/air-gap dial-time logic in `safeurl/safeurl.go`, `tools/terminal.go`'s `Sandbox.MustRefuse()` call, prompt-cache mechanics, and the GPU `-ngl` layer-count calculation) were resolved during consolidation. **Where this file and the consolidated report disagree, the consolidated report is authoritative.**


Scope: `orchestrator/` (60 files, 10.2k LOC) + satellites `loop/`, `agents/`, `dag/`, `plan/`,
`ctxengine/`, `adjudicate/`, `datasource/`, `concurrency/` (5.9k LOC combined).
FACT = cites a file; INFERENCE = interpretation; UNKNOWN = unresolved.

## Purpose
The single execution engine behind every user turn on every surface. Owns planning, the
cost-ascending "cognition cascade," DAG-based task decomposition/execution, an alternate
ReAct-style agentic loop, multi-model consensus, context compression triggers, and outcome
recording into memory.

## Entry Points
- `orchestrator.New(cfg Config, rtr *router.Router, reg *tools.Registry, mem core.MemoryStore,
  comp contextCompressor, emitter *ui.EventEmitter) *Kernel` — `orchestrator/kernel.go:281`.
  Constructed once in `app_wireup.go:initKernelAndServer`.
- `(*Kernel).Execute(ctx, userGoal string) (string, error)` — `orchestrator/kernel_execute.go:158`.
  **This is the ONLY sanctioned entry point** per `.arch-baseline`'s `kernel-entry 0` ceiling —
  reached exclusively through `uiport.Manager.Execute` (verified: `uiport.Engine` interface
  requires exactly this signature, `uiport/uiport.go:61`).

## Public API
Exported `Kernel` setters wired once at startup (`app_wireup.go`): `SetClientFactory`,
`SetRepoRules`, `SetChangeRecorder`, `SetCheckpoints`, `SetLocalLoader`, `SetCascadeLogPath`,
`SetRunsDir`, `SetCostGovernor`, `SetDebate`, `SetReviewer`, `SetRecall`. Runtime query methods:
`Status()`, `CascadeLog()`, `CascadeStats()`, `RecentSTM()`.

## Internal Components
- `kernel.go` (876 lines) — `Kernel` struct, config, mode selection (`resolveRoutingMode`,
  line ~674-683 dispatches on `core.RouteConsensus`/single/escalation).
- `kernel_execute.go` (487 lines) — the `Execute` control flow (see below).
- `kernel_helpers.go` (516 lines) — degraded-mode handling (tools unreachable this turn,
  `kernel_helpers.go:273`), consensus fallback for the no-tools path.
- `cascade.go` (513 lines) — the self-calibrating cognition cascade (see Business Rules).
- `dag_executor.go` (346 lines) — runs a `plan.Graph` as a `dag.DAG`, merges results, verifies,
  repairs failed acceptance criteria, records outcome.
- `memory_recorder.go` (486 lines) — `recordOutcome`: episodic + learning + audit + KG writes
  after every turn.
- `plan_gate.go` (285 lines) — plan-approval gate; consults the knowledge graph's
  `BlastRadius(files, depth)` (`plan_gate.go:65`) to decide whether an edit needs approval even
  under permissive settings.
- `deep_planner.go` (218 lines) — decomposes a goal into a `plan.Graph` (the `dag/` and `plan/`
  packages hold the DAG/graph types themselves; orchestrator holds the planning + execution glue).
- `consensus.go` — `runConsensus`, `runConsensusOnOutput`, `mergeWithConsensus`,
  `adjudicateCtx` (imports `github.com/darkcode/adjudicate` — the "code graph adjudicates" claim).
- `reviewer.go` (169 lines) — optional post-acceptance advisory review, off by default, cannot
  fail a run (see Business Rules — real historical bug documented here).
- `skill_extractor.go` (226 lines) — turns successful outcomes into procedural-memory skills.
- `execlog.go` (266 lines) — the scrubbable event log (README's "ordered event log... scrubbed
  through afterwards").
- `contract.go` (309 lines) — `loop.Contract`/acceptance-criteria plumbing between planning and
  the agentic loop.
- `reflection.go` (207 lines) — self-reflection logic, tested by `reflection_test.go` (310 lines).
- Satellites: `loop/` = the ReAct Sense-Think-Act engine; `agents/` = agent role definitions;
  `dag/` = generic dependency-graph executor; `plan/` = the `Graph`/`Node` plan data model;
  `ctxengine/` = context-window accounting used by compression triggers; `adjudicate/` = consensus
  claim-verification against the datasource/graph; `datasource/` = read-routing abstraction over
  memory (the recall package's read-side counterpart, per its own doc comment); `concurrency/` =
  **empty** (`go list` shows zero imports/exports — a placeholder package, not yet used).

## Dependencies
`orchestrator` imports ~21 darkcode packages (highest fan-out after `server`/`cli`): `adjudicate`,
`agents`, `checkpoint`, `concurrency`, `config`, `core`, `ctxengine`, `dag`, `datasource`, `hooks`,
`loop`, `metrics`, `modelport`, `permission`, `plan`, `recall`, `router`, `safeurl`, `tools`, `ui`.
Notably it does NOT import `memory` or `llm` directly — memory access goes through the
`core.MemoryStore` interface parameter and `recall.Manager` (injected via `SetRecall`), and model
calls go through `router`/`modelport`, consistent with the layering the `.arch-baseline` enforces.

## Dependents
`main` (root), `server`, `cli` — i.e. only the two HTTP/CLI wiring points and root construct a
`Kernel`; everything else reaches it through `uiport`.

## Data
No persistent store of its own besides: `cascade_log.jsonl` (append-only telemetry, path set by
`SetCascadeLogPath`), `runs/` dir (`SetRunsDir` — DAG run journaling for crash-resume, see below).
All durable memory (episodic/semantic/procedural/KG) is written via `core.MemoryStore`/`recall`,
not directly.

## Control Flow — one full turn (`Execute`, `kernel_execute.go:158`)
1. **Cost governor check** (`gov.Check()`) — blocks or warns before any work if a spend cap is hit.
2. **STM append** — user message added to short-term memory.
3. **Pending-plan gate** (`handlePendingPlan`) — if a plan is awaiting approve/reject/revise, this
   turn IS that decision, checked before the cascade so a cached answer can't swallow "approve".
4. **Cognition cascade** (`runCascade`) — tries deterministic tools → answer cache → knowledge
   graph → episodic recall, in that order, before any LLM call (see Business Rules for the
   self-calibration mechanism — this is real and unusually sophisticated).
5. Project-plan/workflow injection into the goal (`injectProjectContext`).
6. **Conditional context compression** — triggers on token-window fullness only (not message
   count — an explicit fix documented in a comment, `kernel_execute.go:217-221`, describing the
   prior message-count heuristic as wrong).
7. Complexity assessment (`router.AssessComplexity`), hybrid recall block fetch.
8. Clarification gate — cold-start vague requests get one clarifying question instead of burning
   tool calls.
9. Chat/general fast paths — read-only or single-worker no-DAG paths for simple/conversational
   questions (a documented historical regression is fixed here too: an earlier "no tools at all"
   fast path caused the agent to falsely claim it couldn't see the filesystem —
   `kernel_execute.go:293-299`).
10. **Agentic loop branch** (if enabled): plans first (to get acceptance criteria/contract) *then*
    loops against the plan — historical bug noted: loop mode used to never see the plan, so its
    stop condition was "the model's own opinion of its own work" (`kernel_execute.go:318-330`).
    On a stuck loop with no plan, escalates by decomposing once. Runs reviewer + consensus
    synthesis afterward if enabled.
11. **Trivial-task fast path** (`executeDirect`) if not decomposition-worthy.
12. **Deep planning** (`deepPlan`) → optional **approval gate** (blast-radius/plan-approval-mode
    dependent) → **DAG execution** (`executePlannedGraph`, `dag_executor.go:19`): run DAG → merge
    results (best-effort merge even on partial cancellation) → verify output
    (`verifyOutput`) → repair failed acceptance criteria by handing them to the loop
    (`repairFailedAcceptance`, explicitly closing the gap where "DAG could prove failure but not
    act on it, loop could iterate but had no target" — `dag_executor.go:87-92`) → record outcome
    (episodic + learning + audit + KG + skill extraction) → emit final output.

## External Effects
No direct DB/file writes besides the cascade log and runs dir; all model calls proxy through
`router`; all memory writes proxy through `core.MemoryStore`/`recall`; all tool execution proxies
through `tools.Registry`.

## Business Rules
- **Self-calibrating cognition cascade** (`cascade.go`): each rung (deterministic/cache/graph/
  recall/LLM) has a per-rung confidence threshold starting at 0.75 that **only ever rises**, never
  falls (`cascade.go:22-25` — "adjustments only ever move toward MORE escalation, because a wrong
  local answer costs trust while an extra LLM call only costs money"). A rung is judged wrong by
  **re-ask detection**: if the user asks a similar question again within an hour
  (`cascadeRetryWindow`, similarity ≥0.6), that's a negative label; once a rung's retried/answered
  ratio exceeds 0.3 across ≥5 samples, its threshold steps up by 0.05, capped at 1.05 (effectively
  disabling it for the session). There's also **fact-level demotion**: a rejected KG-graph-rung
  answer demotes the *specific* source fact nodes (delta -0.15, floor 0.3) rather than only the
  whole rung, so one bad fact doesn't punish unrelated good ones sharing the rung
  (`cascade.go:64-78`). This is genuinely more sophisticated than the README's simple "cost-
  ascending ladder" description implies — it's an online-learning calibration system, not a
  static ladder.
- **Reviewer cannot fail a run** (`reviewer.go:11-16`): explicitly designed so a review "cannot
  turn a proven pass into a failure... the whole point of proving completion mechanically was to
  stop opinions deciding it." Off by default (cost).
- **Real, documented historical defect**: `SetReviewer` had no caller outside tests until fixed —
  "173 lines wired into the execute path were unreachable in a shipped binary" (`app_wireup.go:
  711-712`). This is exactly the class of bug `.arch-baseline`'s `unwired-kernel-setters 0` ceiling
  now guards against (confirms the arch-check metric targets a real, previously-shipped bug class,
  not a hypothetical one).
- **Blast-radius plan gate**: `plan_gate.go:65` — `k.data.BlastRadius(files, 2)` (depth 2) feeds
  the approval decision, confirming the README's "shown in the plan approval gate before you
  approve" claim against real code, not just doc.
- **Consensus/adjudication**: `runConsensus`/`mergeWithConsensus` fan out to multiple models when
  `router.GetMode() == core.RouteConsensus && router.ModelCount() > 1`; `adjudicateCtx` pulls in
  the separate `adjudicate` package. Did not read `adjudicate/`'s internals in depth — see
  Unknowns for exactly how "claims survive verification" is implemented (whether it's graph-node
  cross-checking or a weaker heuristic).
- **DAG crash-resume** (`SetRunsDir`): app_wireup comment claims "a crashed multi-step task
  resumes from where it stopped instead of re-paying for every completed sub-task" — I located the
  wiring (`a.Kernel.SetRunsDir(...)`) but did not trace the resume-read path inside `dag_executor.go`
  or `dag/` in enough depth to confirm actual resume-from-partial-state logic executes on startup
  vs. only journaling for post-hoc inspection. **UNKNOWN — flagged for follow-up.**

## State
`Kernel` holds `k.mu sync.Mutex`-guarded fields: `projectPlan`/`projectWorkflow` (per-request,
set/cleared around `Execute` calls), `cascadeLog`/`cascadeThresholds`/`cascadeRungAnswered`/
`cascadeRungRetried` (session-lifetime, capped at 200 in-memory entries, `maxCascadeLog`), 
`lastRunPlan`, `reviewerOn`, `governor`.

## Concurrency
- All kernel mutable state behind one `sync.Mutex` (not RWMutex) — every setter and every read
  path (`rungThreshold`, `CascadeStats`, etc.) takes the same lock, which is coarse but simple;
  plausible contention point under `MaxConcurrent` concurrent turns (not measured).
- `MaxConcurrent` (config field) governs DAG sub-task fan-out concurrency inside `dag/`'s executor
  — did not read `dag/dag.go` internals to confirm the actual goroutine-pool mechanism.
- The `uiport.go` package comment (already read by the parent) states Mode/Safety/Brain request
  overrides "mutate shared router/gate state under a depth counter" — implying **the router and
  permission gate are shared, request-scoped-only-by-convention mutable state**, not per-request
  isolated instances. This is a real concurrency design point: two concurrent requests with
  different Safety/Brain overrides could interact through this shared state if the depth-counter
  mechanism (not yet located — likely `orchestrator/request_state.go`) has a bug. **Flagged as a
  risk area**, not confirmed broken — the depth-counter is explicitly built to guard against
  exactly this.

## Error Handling
- Cost governor: hard block (return error) vs. warn-and-proceed, per configured `Action`.
- Planning failure always falls back to direct execution rather than failing the turn
  (`kernel_execute.go:461-463`).
- DAG partial failure still produces a best-effort merged answer from completed sub-tasks, labeled
  `[Partial result...]`, rather than discarding all completed work (`dag_executor.go:40-54`).
- Failed/blocked DAG nodes are surfaced honestly in the final output text, not swallowed
  (`dag_executor.go:66-77`) — a documented fix for a prior "reads as a clean success" problem.

## Tests
Substantial: `kernel_execute_test.go` (243), `dag_executor_test.go` (298), `consensus_test.go`
(213), `cascade_test.go` (193), `plan_gate_test.go` (172), `reflection_test.go` (310). Broad
coverage of the control-flow branches described above. Did not enumerate what's *missing*
(e.g., concurrent-turn interaction tests for the shared router/gate state noted above) — plausible
gap given the single-turn framing of most test names, but not confirmed absent.

## Important Files
| File | Purpose |
|---|---|
| `orchestrator/kernel.go` | Kernel struct, construction, routing-mode resolution |
| `orchestrator/kernel_execute.go` | Main `Execute` control flow — the spine of the system |
| `orchestrator/kernel_helpers.go` | Degraded-mode / tools-unreachable handling |
| `orchestrator/cascade.go` | Self-calibrating cognition cascade |
| `orchestrator/dag_executor.go` | Plan-graph → DAG execution → merge → verify → repair |
| `orchestrator/memory_recorder.go` | Outcome recording (episodic/learning/audit/KG) |
| `orchestrator/plan_gate.go` | Plan approval + blast-radius escalation |
| `orchestrator/deep_planner.go` | Goal → plan.Graph decomposition |
| `orchestrator/consensus.go` | Multi-model fan-out + adjudicated synthesis |
| `orchestrator/reviewer.go` | Optional non-blocking post-acceptance review |
| `orchestrator/skill_extractor.go` | Successful-outcome → procedural skill extraction |
| `orchestrator/execlog.go` | Scrubbable per-run event log |
| `orchestrator/contract.go` | Acceptance-criteria contract for the agentic loop |
| `loop/` (package) | ReAct Sense-Think-Act execution engine |
| `dag/` (package) | Generic dependency-ordered task executor |
| `plan/` (package) | Plan graph data model |
| `adjudicate/` (package) | Consensus claim verification |

## Unknowns
- Exact mechanism of `orchestrator/request_state.go`'s depth counter for per-request Mode/Safety/
  Brain overrides on shared router/gate state — not opened.
- Whether DAG crash-resume (`SetRunsDir`) actually resumes execution or only journals for
  inspection.
- Internals of `adjudicate/` — how rigorous "claims survive verification" actually is.
- `concurrency/` package is empty (zero imports per `go list`) — dead placeholder or in-progress
  work; not investigated further, low priority.
- Whether `MaxConcurrent` is enforced via a semaphore/worker pool inside `dag/` or is advisory —
  not confirmed.

## Claim verification table
| Claim | Verdict | Evidence |
|---|---|---|
| "Cognition Cascade" — cost-ascending local answerers before any LLM call | VERIFIED, and more sophisticated than described | `cascade.go` — real online-calibration with re-ask detection, per-rung and per-fact threshold adjustment |
| Sub-agent roles executive/planner/worker/critic (README) | PARTIALLY VERIFIED | `core.RoleWorker` confirmed used (`kernel_execute.go:427`); reviewer.go independently documents SIX personas (critic, skeptic, verifier, analyst, creative, knowledge_booster) for consensus, not the README's 5-role list — the two descriptions of "roles" don't fully match; did not exhaustively enumerate `agents/` package roles |
| DAG-based task execution, resumable runs | PARTIALLY VERIFIED | DAG execution confirmed real and sophisticated (wave-based, partial-failure recovery); crash-resume specifically UNKNOWN |
| Consensus "adjudicated by the code graph" | PARTIALLY VERIFIED | Real `adjudicate` package exists and is wired via `adjudicateCtx`; depth of graph-grounding not confirmed |
| "SetReviewer had no caller... 173 lines unreachable" (wireup comment) | VERIFIED as real historical defect | `app_wireup.go:711-712`, corroborated by `.arch-baseline`'s `unwired-kernel-setters 0` ceiling existing specifically to catch this bug class |
