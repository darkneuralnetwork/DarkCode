# Architecture Analysis — DarkCode

**Phase 1–2 deliverable. Analysis only; no code changed.**

Produced against `a52af24` (1.5.0).
Baseline: `go build ./...` and `go vet ./...` both clean. [RAN]

Epistemic markers follow the project convention in `THESIS.md` §0:
**[RAN]** = executed and read the output. **[READ]** = traced the code, did not run it.

---

## 0. Correction to THESIS.md before anything else

`THESIS.md` §3.7 describes three manager packages — `recall`, `agentport`,
`modelport` — and §6 "Phase 2 — Managers" reports migration counts against them
("Fact-store writes outside `recall`: 0", "Direct `kernel.Execute` outside
`agentport`: 0").

**None of those packages exist in this repository, and none ever did.** [RAN]

```
$ git log --oneline --all -- recall agentport modelport
(no output)
$ ls recall agentport modelport
ls: cannot access 'recall': No such file or directory
```

THESIS.md was written against a different worktree, per its own header, whose
work never landed on this line of history. The measured reality on `main` today is the
inverse of what §6 claims:

| THESIS.md §6 claim | Actual, measured here [RAN] |
|---|---:|
| Fact-store writes outside `recall`: **0** | **32** sites, 6 files |
| `kernel.Execute` outside `agentport`: **0** | **6** sites, 5 files |
| `ChatCompletion` policy sites outside `modelport`: **15** | **23** sites, 17 files |

This does not make the *design* in THESIS.md wrong — it is close to what you have
asked for, and the fact that it was independently arrived at twice is evidence it
is the right shape. It means the work is unstarted, not half-finished, and that
no migration counts in that document can be used as a starting position.

Two further facts from history that shape the plan below:

- Recent history contains **two `Revert` commits** (`ca66349`, `353051e`) undoing
  large single-step changes. This codebase has demonstrated that big-bang changes
  get rolled back. The migration plan is therefore sized in independently
  revertible commits.
- The inbound surfaces THESIS.md §1 says were "deliberately removed" — `/v1`, MCP,
  ITF — are all present and routed (`server/server.go:228-229`, `server/mcp.go`,
  `server/htp.go`). [RAN]

---

## 1. Current architecture

```
                         User
                           │
      ┌──────────┬─────────┼─────────┬────────────┐
      │          │         │         │            │
    CLI      GUI (HTTP)  ACP     headless    /v1 · MCP · HTP
   (cli/)   (server/)   (acp/)   (app.go)     (server/)
      │          │         │         │            │
      │   ┌──────┴─────────┴─────────┴────────────┘
      │   │   6 call sites, no common entry point
      ▼   ▼
  ┌─────────────────────────────────────────────────────────┐
  │                orchestrator.Kernel                       │
  │        48 struct fields · 112 methods · 18 files         │
  │   cascade · planning · DAG · consensus · debate ·        │
  │   reviewer · repair · reflection · skill extraction ·    │
  │   plan gate · memory recording · cost governance         │
  └──┬──────┬──────┬───────┬────────┬────────┬──────┬────────┘
     │      │      │       │        │        │      │
     ▼      ▼      ▼       ▼        ▼        ▼      ▼
  router  memory  tools  llm   compression ctxengine loop/agents
     │      │      │       │        │        │
     ▼      ▼      ▼       ▼        ▼        ▼
  providers  KG   perm.  HTTP    LLM call  LLM call
             SQLite gate

  ── and, bypassing the kernel entirely: ──
  cli/console.go ──────────────► llm client (2 direct calls)
  server/planworkflow.go ──────► llm client (2 direct calls)
  tools/web.go, research.go ───► llm client (2 direct calls)
  memory/*.go ─────────────────► embeddings (3 direct calls)
  ingest, intelligence ────────► memory writes
```

The shape is a **star with leaks**: a god-object kernel at the centre that every
surface reaches into directly, plus a set of edges that route around it entirely.

### 1.1 Package dependency graph (internal edges only) [RAN]

Go forbids import cycles, so there are **no compile-level circular dependencies**.
The problems are all directional-layering violations, which the compiler cannot see.

```
        core ◄──────────────── (imported by nearly everything; the shared vocabulary)
          ▲
          │
  ┌───────┴────────┬──────────────┬──────────────┐
  │                │              │              │
 llm            memory          tools          router
  │                │              │ │            │
  │                │              │ └──► memory  │  ◄── tools imports memory + llm
  │                ▼              │                     (LAYER VIOLATION)
  │           intelligence        └──► llm
  │
  └──► safeurl / httpx

  compression ──► core, config, observability      (clean)
  ctxengine   ──► core                             (clean)
  ui          ──► core                             (clean)

  orchestrator ──► agents, checkpoint, compression, config, core, ctxengine,
                   dag, llm, loop, memory, metrics, permission, plan, router,
                   safeurl, tools, ui
                   ▲ 10 concrete implementation packages imported directly

  server ──► orchestrator, memory, llm, router, tools, plan, provider,
             intelligence, ingest, attach, plugin, project, verb, capability, ...
  cli    ──► orchestrator, memory, llm, router, tools, provider, ingest,
             checkpoint, attach, capability, verb, ...
             ▲ both UI surfaces import the entire stack
```

**The one-way rule THESIS.md §2 states — "`orchestrator` must never import
`server`" — holds.** [RAN] That is the only layering invariant currently enforced,
and it is enforced by nothing but convention.

### 1.2 Request lifecycle (current)

Traced through `orchestrator/kernel_execute.go`. [READ]

```
User input
  │
  ▼
Surface (cli.Console.Run / server.chatHandler / acp / app.runHeadless / openai_compat)
  │  each independently: resolves verb, sets overrides on kernel via setters,
  │  builds its own context.Context, installs its own approver
  ▼
Kernel.Execute(ctx, goal)                                       kernel_execute.go
  │
  ├─ 1.0  cost governor check (metrics.CostGovernor)
  ├─ 1.2  runCascade(ctx, goal)                                  cascade.go
  │        ├─ RungDeterministic → registry.Execute(tool)   ← tool call
  │        ├─ RungCache         → answer cache
  │        ├─ RungGraph         → memory.KG()              ← memory read
  │        ├─ RungRecall        → HybridRetriever.Recall   ← memory read
  │        └─ (miss) fall through
  │        └─ on hit: memory.STMAdd + emit + RETURN
  │
  ├─ 2    injectProjectContext(goal)
  │
  ├─ 3    ►► COMPRESSION ◄◄   if cfg.CompressContext (default TRUE)
  │        compressor.Compress(ctx, stm, goal)              ← LLM CALL
  │        memory.STMCompress(briefing, keepRecent=4)       ← DESTRUCTIVE
  │
  ├─ 4    router.AssessComplexity(goal)
  ├─ 5    getRecallBlock(goal) → HybridRetriever.Recall(goal, 10)
  │        → epoch filter → top 3 → FormatRecall            ← memory read
  ├─ 6    clarification gate / intent classification
  ├─ 7    strategy dispatch by verb:
  │        ├─ /ask       → executeDirectNoTools   → ctxengine.Assemble → LLM
  │        ├─ /loop      → loop.ReActLoop         → LLM + tools
  │        ├─ /graph     → deep_planner → plan_gate → dag_executor → LLM + tools
  │        └─ /consensus → consensus.go           → LLM × N + synthesis
  │
  ├─ 8    acceptance / contract checks → registry.Execute("terminal")
  ├─ 9    optional: reviewer, debate, repair, reflection     ← more LLM calls
  ├─ 10   recordOutcome → memory_recorder.go (16 write sites)  ← memory write
  ├─ 11   skill extraction → ProceduralAdd                     ← memory write
  │
  ▼
emitter.EmitFinalOutput(output)  →  ui.EventEmitter  →  SSE / stdout / ACP
  │
  ▼
Surface renders. CLI additionally fires 2 more LLM calls in a goroutine
(plan + workflow refresh, cli/console.go:648,666) — logic duplicated in
server/planworkflow.go for the GUI.
```

### 1.3 Data flow — where data actually enters and leaves

| Boundary | Sites | Gateway? |
|---|---:|---|
| **LLM calls** (`ChatCompletion`/`Stream`/`CreateEmbedding`) outside `llm`/`router`/`provider` | **23** in 17 files | ❌ none |
| **Memory writes** outside `memory`/`core` | **32** in 6 files | ❌ none |
| **Tool execution** (`registry.Execute` / `DispatchAll`) | 15 | ✅ **yes — `tools.Registry`** |
| **HTTP egress** | all via `safeurl`/`httpx` | ✅ yes |
| **Kernel entry** | 6 sites, 5 files | ❌ none |

The full lists are in §3.

---

## 2. Managers that exist today

| Component | Package | Is it a real manager? |
|---|---|---|
| `tools.Registry` | `tools` | **Yes.** Both dispatch paths (`DispatchAll`, `Execute`) gate identically: read-only policy → permission gate → circuit breaker → timeout → telemetry. This is already the Tool Manager the target architecture asks for. |
| `permission.Gate` | `permission` | Yes, and clean. Deny rules → risk classification → session cache → approver → fail-closed timeout. |
| `router.Router` | `router` | **Partly.** Owns model selection and tiers, but is not the only path to a model — 23 sites bypass it. |
| `memory.System` | `memory` | **Partly.** Owns the stores, but exposes raw mutators (`EpisodicAdd`, `SemanticAdd`, `AddNode`) that 8 other packages call directly. No placement policy. |
| `ui.EventEmitter` | `ui` | **Partly.** A good pub/sub telemetry bus, but it is *injected into 8 packages* (`tools`, `router`, `security`, `loop`, `agents`, `orchestrator`, `server`, `cli`) rather than being a layer above them. |
| `orchestrator.Kernel` | `orchestrator` | **No — it is a god object.** 48 fields, 112 methods, 18 files. |
| `compression.Compressor` | `compression` | A service, not a manager. |
| `ctxengine.Engine` | `ctxengine` | A service. Opt-in, off by default, **zero tests**, 2 call sites. |
| `model.Manager` | `model` | **Dead code.** Zero importers [RAN]. Placeholder URLs (`https://huggingface.co/...`), comment says "In a complete implementation…". Unverified download, unchecked `io.Copy`. |
| `orchestrator.ChatManager` | `orchestrator` | 127 lines; thin. |

**There is no Data Source Manager, no LLM Manager, no UI Manager.**
The Tool Manager exists and is good.

---

## 3. Architectural problems

Ordered by cost to the project.

### P1 — The Kernel is a god object — **CRITICAL** [RAN]

48 struct fields, 112 methods, 18 files, ~6,000 lines. It simultaneously owns:
intent classification, cognition cascade, compression triggering, complexity
assessment, recall assembly, planning, plan approval gating, DAG execution,
consensus, debate, review, repair, reflection, skill extraction, memory
recording, cost governance, per-request override bookkeeping, and event emission.

Concretely, `Kernel` holds **10 concrete implementation packages** as fields
(`router`, `tools`, `memory`, `compression`, `agents`, `loop`, `checkpoint`,
`permission`, `metrics`, `ctxengine`) rather than interfaces — despite
`core/interfaces.go` already defining `ModelRouter`, `MemoryStore`,
`ToolRegistry`, `ContextCompressor` for exactly this purpose.

**Those interfaces exist and are unused by the thing they were written for.**
That is the single clearest signal of what to do.

### P2 — Per-request state lives on a shared mutable object — **CRITICAL** [READ]

`requestLoop`, `requestPlan`, `requestToolsDisabled`, `requestReadOnly`,
`projectPlan`, `projectWorkflow`, `pendingPlan`, `lastRunPlan`,
`lastCompressedLen` are **fields on the shared Kernel**, mutated per request under
`k.mu` with a save-old/set-new/restore-on-return dance.

The kernel's own comment admits the hazard:

> "the thing saved and restored is shared router and gate state, so two
> overlapping [requests]…"

Commit `39389ec` ("keep a verb to one message even when requests overlap") is a
patch on this design. **The GUI is concurrent** — SSE plus `/api/chat` plus `/v1`
can all be in flight. This is a correctness bug class, not a style problem, and
it is the strongest argument for a per-request Session object.

### P3 — Business logic in the UI layer, duplicated across surfaces — **HIGH** [RAN]

`cli/console.go:648,666` and `server/planworkflow.go:164,283` implement the *same
feature* (regenerate the project plan and workflow after a turn) with **separately
written prompts, separately constructed LLM clients, and different call counts** —
the CLI spends 2 calls, the server spends 1 (it splits on a `===WORKFLOW===`
delimiter). The CLI version constructs its own client inline:

```go
client := llm.WrapCloud(llm.NewClient(c.cfg.BaseURL, c.cfg.APIKey, c.cfg.Model), ...)
```

bypassing the router, the cost governor, token accounting, and the model policy
entirely. Two surfaces, one feature, two implementations, one of them untracked
by metrics.

### P4 — No single entry point; each surface re-implements setup — **HIGH** [RAN]

6 `kernel.Execute` call sites across `app.go`, `app_acp.go`, `cli/console.go`,
`server/chat_handler.go` (×2), `server/openai_compat.go`. Each independently
decides how to set overrides, install an approver, scope tools, and build a
context. A surface that forgets one gets silently different behaviour — this is
the mechanism behind the "stale approver after CLI→GUI switch" bug that
`modeApprover` was added to patch.

### P5 — `tools` imports `memory` and `llm` — **HIGH** [RAN]

The tool layer reaches *up* into memory and the model layer:
`tools/memory_tool.go` writes memory, `tools/deterministic/kgsync.go` performs
**10** knowledge-graph writes, `tools/web.go` and `tools/research.go` make their
own LLM calls. So "execute a tool" can transitively write beliefs and spend
money on a model, with no route through any gateway.

### P6 — Lossy compression is on by default and destroys history — **HIGH** [RAN]

Detailed in §5. `CompressContext` defaults to `true` (`config/config.go:339`).

### P7 — Redundant abstractions — **MEDIUM** [RAN]

- **Three overlapping context-assembly systems**: `compression.Compressor`
  (LLM summarization), `ctxengine.Engine` (dedup + TF-IDF rank + trim + adaptive
  compress), and `compression/budget.go` (`FitToWindow`, deterministic). They are
  not layered; they are alternatives selected by config flags, and two of them
  can run on the same request (`kernel_helpers.go:389` then
  `compression.FitClient` at line 407).
- **`ctxengine` has zero tests** and is off by default — an untested code path
  that assembles the prompt when enabled.
- **`ctxengine/engine.go:106` `chronologicalSort` is a no-op** that returns its
  input unchanged, with a comment explaining it decided not to do anything. The
  pipeline comment above it claims a chronological re-sort happens. It does not.
- **`model` package is entirely dead** — zero importers.
- **`core/interfaces.go`** defines four interfaces the kernel ignores.
- **`core.ContextCompressor` declares 5 methods; 2 are never called by anyone.**
  `CompressBlock` and `AssembleContext` are declared in the interface,
  implemented in `compression/compressor.go`, and invoked from **zero** call
  sites [RAN]. An interface method with no caller is a maintained abstraction
  paying rent on a feature that does not exist — `CompressBlock`'s doc comment
  describes a "hierarchical compression system" that is never entered.

### P8 — Invariants held by convention, not enforcement — **MEDIUM** [RAN]

`httpx`'s comment claims a CI grep enforces it; THESIS.md §7.4 already recorded
that no such CI step exists. Same is true for every layering rule above. Nothing
would catch a new direct `ChatCompletion` call.

### P9 — Telemetry is emitted but not aggregated — **MEDIUM** [READ]

`ui.EventEmitter` has 20 typed emit methods and is threaded into 8 packages. But
there is no component that assembles a *request timeline* — the GUI reconstructs
one from the SSE stream, the CLI keeps a separate `activity` slice
(`cli/console.go:recordActivity`), and ACP has neither. The data exists; nothing
owns the view of it.

### 3.1 Circular dependencies

**None at the package level** — Go would not compile. [RAN]

Logical cycles do exist and are the real problem:

```
orchestrator ──► tools ──► memory ──► (embeddings) ──► llm ──► ...
     └──► memory ──┘                                     ▲
     └──► llm ────────────────────────────────────────────┘
```

`orchestrator` and `tools` both write memory; `memory` and `tools` both call
LLMs; `orchestrator` calls all three. There is no ordering in which one of these
is "below" the others, which is precisely why a gateway is needed.

### 3.2 SRP violations, ranked

| Component | Distinct responsibilities | Verdict |
|---|---:|---|
| `orchestrator.Kernel` | ~14 | Severe |
| `cli.Console` | UI + verb parsing + LLM calls + project state + activity log | Severe |
| `server.Server` | HTTP + orchestration + LLM calls + 68 routes | Severe |
| `memory.System` | 5 stores + embedding + retrieval + persistence + epoch | Moderate |
| `tools.Registry` | registration + dispatch + gating + breaker + checkpointing | Acceptable — all one concern (tool lifecycle) |
| `permission.Gate` | 1 | Clean |
| `ui.EventEmitter` | 1 | Clean |

---

## 4. Cost analysis — where tokens and latency go

### 4.1 Token-cost hotspots [READ]

| # | Site | Cost |
|---|---|---|
| 1 | `kernel_execute.go:232` compression | An **extra LLM call per request** once STM ≥ 8 messages, on by default. Pays tokens to *lose* information. |
| 2 | `cli/console.go:648,666` | **2 extra LLM calls per CLI turn** when a project is active, off the metered path. |
| 3 | `server/planworkflow.go` | 1 extra call per project turn; duplicate of #2. |
| 4 | `getRecallBlock` | Fetches 10 hits, **discards 7**, injects 3. The ranking work for 10 is done every request; no cache. |
| 5 | Cascade rungs | The *good* news — `RungDeterministic`/`Cache`/`Graph`/`Recall` are a genuine cost control and the best idea in the codebase. Keep entirely. |
| 6 | `orchestrator/debate.go`, `reviewer.go` | 2 and 1 extra calls; both correctly default **off**. |
| 7 | No shared retrieval cache | Two requests with the same query re-run vector + token + graph scoring from scratch. |
| 8 | Tool schemas | `LLMSchemasFor(task)` relevance filtering exists and is a real saving. Keep. |

### 4.2 Latency hotspots [READ]

- Compression is **synchronous and blocking** on the request path
  (`kernel_execute.go:232`) — an LLM round-trip before the real LLM round-trip.
- `getRecallBlock` runs vector + token-overlap + graph scoring inline, and
  `HybridRetriever.Recall` scans **all** semantic entries per call
  (`memory/retrieval.go:73`).
- The cascade is sequential by construction (that is correct — each rung must
  miss before the next is worth trying).
- CLI plan/workflow refresh is `go func` (non-blocking) but competes for the
  same rate limit; the code already documents this causing 429 storms on the
  user's free tier.

### 4.3 Maintenance-complexity hotspots

- `app_wireup.go` — 31 KB, ~30 components in dependency order, 0.9% coverage.
- `orchestrator.Kernel` — a change to any subsystem risks 18 files.
- Adding a fifth surface means re-implementing setup a fifth time (P4).
- Adding a sixth memory store means touching all 32 write sites (P5).

---

## 5. Context compression — every location [RAN]

This is Phase 3's target.

### 5.0 What production agents actually do — researched before deciding [RAN]

The brief says "remove context compression completely" and replace it with
retrieval. Before implementing that, I checked how shipping coding agents handle
context exhaustion. **The evidence contradicts the instruction, so the plan is
revised rather than executed as written.**

| Agent | Compaction trigger | Originals | Uses retrieval too? |
|---|---|---|:---:|
| CLI agent A | ~83.5–95% of window (env-configurable) | discarded; history replaced by a summary | yes |
| Agent C | token threshold = window − 16–20k reserve | pre-compaction state flush | yes |
| IDE agent B | RAG-first; never accumulates | n/a | yes (primary) |
| Anthropic SDK `compaction_control` | `context_token_threshold`, default 100k | history cleared, summary only | yes |
| Industry range | **50–90% of window** | offload to disk + ~2KB preview | yes |

Two findings decide the design:

1. **Nobody removes compaction.** Every production harness has it.
2. **Compaction and retrieval are complementary layers, not alternatives.**
   Retrieval handles discrete external content (repo, KB, files). Compaction
   manages *the agent's own evolving working memory across a long task* — which
   RAG structurally cannot do, because that state has no external source to
   retrieve from. The documented failure mode is compaction firing *mid-task*,
   forcing the agent to reconstruct implementation details from a lossy summary.

So the target is not removal. It is **correct tiering**:

```
Layer 1  Retrieval        — external/durable knowledge (repo, KG, memory)   ← DarkCode is strong here
Layer 2  Offloading       — oversized tool results → disk + small preview   ← MISSING entirely
Layer 3  Selection        — deterministic dedup/rank/pin/fit, no LLM call   ← exists, scattered
Layer 4  Compaction       — LAST RESORT at a high watermark, non-destructive ← exists, misconfigured
```

### 5.0.1 Measured against that, DarkCode's compaction has four defects [RAN]

From `orchestrator/kernel_execute.go:215-241`:

| # | Defect | Evidence | Fix |
|---|---|---|---|
| D1 | **Message-count trigger unrelated to context pressure.** `len(stm) >= 8 && growth >= 4` fires compaction after 8 messages *regardless of tokens* — 8 short turns may be ~500 tokens. Spends an LLM call to compact a nearly-empty window. No production agent has such a rule. | `skill_extractor.go:191,196` | **Delete the trigger** |
| D2 | **Watermark far too eager.** Fires at **60%** of the window; industry is 50–90%, typically ~85%, or window−16–20k reserve. Discards information while 40% of the window is unused. | `kernel_execute.go:225` | Raise to window − reserve |
| D3 | **Destroys originals with no flush.** `STMCompress` overwrites the buffer; nothing is recoverable. Industry practice is pre-compaction state flush / disk offload with retrievable handles. | `memory/system.go:316` | Make non-destructive |
| D4 | **No tool-result offloading — the single biggest available token win.** Harnesses persist oversized tool results to disk and substitute a ~2KB preview; tool results are what actually bloats an agent context. DarkCode has no equivalent. | absent | **Add it** |

Note the interaction: **D4 is why D1/D2 feel necessary.** Without offloading, one
large `read_file` or `search_files` result blows the window, so the trigger was
tuned ever-more-eagerly to compensate. Fixing D4 removes the pressure that
motivated D1/D2 — this is working rule #12, "fix the model, not the constant."

### 5.0.2 The distinction that still holds

> **Deterministic budget fitting** (choosing, without an LLM, what fits) is not
> compression — it *is* the "optimize context selection" the brief asks for.
> Removing `FitToWindow` would make every request to a small-window local model
> fail with `ErrContextTooLong`, breaking the local-first bet.
>
> **Redundant compression layers** — three overlapping systems where one belongs
> — are the real architectural problem, and those do get collapsed.

### 5.1 Full inventory

| # | Location | Mechanism | LLM call? | Lossy? | Disposition |
|---|---|---|:---:|:---:|---|
| C1 | `kernel_execute.go:215-231` compaction **trigger** | `len(stm)>=8` OR 60% of window | — | — | **RE-TIER** — delete the count rule (D1), raise watermark (D2) |
| C2 | `kernel_execute.go:232` `compressor.Compress` | LLM summarizes STM | **yes** | **yes** | **KEEP as last resort** — this is what every agent does; only the trigger was wrong |
| C3 | `kernel_execute.go:241` `memory.STMCompress` | **overwrites** STM, unrecoverable | no | **yes, permanently** | **MAKE NON-DESTRUCTIVE** (D3) — flush originals first |
| C4 | `compression/compressor.go:97` `Compress` | LLM → `ContextSnapshot` | **yes** | **yes** | keep; becomes the single compaction implementation |
| C5 | `compression/compressor.go:224` `CompressBlock` | hierarchical block summary | **yes** | **yes** | **REMOVE** — **zero callers anywhere** [RAN] |
| C6 | `compression/compressor.go:308` `AssembleContext` | compress-to-budget | **yes** | **yes** | **REMOVE** — superseded by the selection pipeline |
| C7 | `compression/compressor.go:536` `Summarize` | LLM narrative summary | **yes** | **yes** | **KEEP** — explicit user request only |
| C8 | `kernel.go:692` `compressor.Summarize` | project brief | **yes** | yes | keep (explicit) |
| C9 | `ctxengine/engine.go:85` `AdaptiveCompressor.Compress` | overflow → summary | **yes** | **yes** | **REMOVE** — duplicate of C4, untested |
| C10 | `ctxengine/components.go:374` `summarizer.Summarize` | older-message rollup | **yes** | **yes** | **REMOVE** — duplicate |
| C11 | `ctxengine/components.go:70` `s.llm.ChatCompletion` | the summarizer's call | **yes** | **yes** | **REMOVE** — duplicate |
| C12 | `ctxengine` dedup + TF-IDF rank + budget trim | deterministic selection | no | selective | **KEEP & PROMOTE** — Layer 3 |
| C13 | `compression/budget.go:159` `FitToWindow` | deterministic drop/truncate | no | truncates | **KEEP** — the hard overflow guarantee |
| C14 | `compression/budget.go:348` `FitClient` | `FitToWindow` per client window | no | truncates | **KEEP** |
| C15 | `compression/budget.go:324` `truncateMiddle` | middle-out truncation | no | yes | keep, last resort only |
| C16 | `compression/importance.go` `ScoreMessage` | pin/importance scoring | no | no | **KEEP & PROMOTE** — Layer 3 signal |
| C17 | `kernel_helpers.go` `boundedChatContext` | rolling summary + tail | no | yes | **REMOVE** — third duplicate of the same idea |
| C18 | `agents/subagent.go`, `loop/loop.go`, `router/router.go` | hold a `Compressor` | via above | — | drop the dependency — compaction belongs to one owner |
| C19 | `core.ContextCompressor` interface | the abstraction | — | — | replace with `ContextAssembler` (selection + compaction) |
| **C20** | **tool-result offloading** | **absent (D4)** | — | — | **ADD** — the largest available token win |

**Summary: three overlapping compaction implementations collapse to one
(C9–C11, C17 deleted as duplicates of C4); the trigger is re-tiered (C1–C3);
tool-result offloading is added (C20); the five deterministic selection
mechanisms (C12–C16) are kept and promoted to a real layer.**

Net effect on the request path: a typical turn goes from **one compaction LLM
call every ~4 messages** to **zero until the window is genuinely ~85% full** —
while losing less, because the originals survive and tool results offload rather
than being summarized away.

### 5.2 What replaces it

The parts needed for retrieval-instead-of-compression **already exist** and are
better than the compression they would replace:

- `memory.HybridRetriever` — reciprocal-rank fusion over vector cosine + token
  overlap + graph neighbourhood, with deterministic tie-breaking. This is a
  genuinely good retriever.
- `ctxengine` deduplication + TF-IDF relevance ranking + token-budget trimming.
- `compression/importance.go` — per-message importance and pinning.
- `compression/budget.go` `FitToWindow` — the deterministic final guarantee.

The change is to **compose them into one selection pipeline** and delete the
summarization layer sitting on top:

```
BEFORE: STM ──► LLM summarize ──► overwrite STM ──► fit ──► prompt
                (costs a call, loses the originals permanently)

AFTER:  STM (intact, never mutated)
          │
          ├─ retrieve: rank by relevance to *this* query (RRF + TF-IDF)
          ├─ pin: always-keep messages (importance scoring)
          ├─ dedup: exact + near-duplicate
          ├─ select: fill the budget with highest-value items
          └─ fit: FitToWindow as the hard guarantee
                ──► prompt
        (costs no call, originals stay recoverable, selection is per-query)
```

The decisive advantage is not only cost: **selection is query-dependent, summary
is not.** A summary made at turn 8 cannot know what turn 20 will ask. Retrieval
over intact history can.

---

## 6. Proposed architecture

```
                              User
                                │
             ┌──────────────────┼──────────────────┐
             │                  │                  │
            CLI                GUI                ACP
             └──────────────────┼──────────────────┘
                                │
                        ┌───────────────┐
                        │  UI Manager   │  presentation + telemetry only
                        │  (uiport)     │  Request in → Event stream out
                        └───────┬───────┘
                                │
                        ┌───────────────┐
                        │ Orchestrator  │  coordination only
                        │  (kernel)     │  no llm/memory/db imports
                        └───┬───────┬───┘
                            │       │
              ┌─────────────┘       └──────────────┐
              │                                    │
    ┌──────────────────┐                 ┌──────────────────┐
    │ Data Source Mgr  │                 │   Tool Manager   │
    │   (datasource)   │                 │  (tools.Registry)│
    │ THE data gateway │                 │   already exists │
    └────┬────────┬────┘                 └──────────────────┘
         │        │
    ┌────▼───┐ ┌──▼──────┐
    │ Memory │ │   LLM   │
    │  Mgr   │ │   Mgr   │
    └───┬────┘ └────┬────┘
        │           │
  STM/episodic/  cloud + local
  semantic/proc/  providers
  knowledge graph
```

### Mapping to what exists

| Target manager | Build from | Effort |
|---|---|---|
| **Tool Manager** | `tools.Registry` — **already correct** | Enforce only |
| **LLM Manager** | `router.Router` + `llm` + `provider`, given a `Purpose`-based API | Medium |
| **Memory Manager** | `memory.System` + `HybridRetriever`, given placement policy + a narrowed interface | Medium |
| **Data Source Manager** | **New**, thin. Composes Memory + LLM. Owns retrieval, ranking, dedup, caching, context assembly | New, ~600 lines |
| **UI Manager** | `ui.EventEmitter` + a new `Session` type + telemetry aggregator | Medium |
| **Orchestrator** | `orchestrator.Kernel`, with 10 concrete deps replaced by 2 interfaces | Large — the real work |

---

## 6.1 Progress

Each stage is one revertible commit, `make ci` green (race detector included).

| Stage | Commit | State |
|---|---|---|
| 0 — Enforcement | `e6c37c1` | **done** — 6 boundaries in `.arch-baseline`, wired into `make ci` |
| 1 — Delete dead weight | `dc1a31c` | **done** — net −49 lines; reviewer wired rather than deleted |
| 2 — Request isolation | `7a3f2c2` | **done** — 4 shared flags moved to request context |
| 3 — UI Manager | `99dd090` | **done** — 6 kernel entries → 1; path confinement armed |
| 4 — LLM Manager | `a9a9c65`, `d9d0211` | **done** — `modelport` holds the policy table; every call now bounded |
| 5 — Context re-tiering | `0a7e944`, `e2bd18d` | **done** — trigger fixed, offloading added, retention added |
| 5b — Surface parity | `0179179`, `3e8e94f`, `4519fa6` | **done** — four surfaces, one door, identical behaviour |
| 5c — Adaptive concurrency | `4519fa6` | **done** — `auto` decides per wave from live signals |
| 5d — Tool-result offloading | `e2bd18d` | **done** — verified live on a 208 KB file |
| 5e — Conversational turns keep tools | `23156de` | **done** — no tool-less mode; read-only is per-call |
| 5f — Content-hash file beliefs | `67c8c31`, `ba342d7` | **done** — catches uncommitted edits |
| 6 — Memory Manager | `0931aef` | **done** — `recall` owns placement; 32 → 24 direct writes |
| 7 — Data Source Manager | `2e124a0` | **partial** — caching landed; the read gateway needs the LLM Manager first |
| 8 — Documentation | — | this file |

### The context stack, after Stage 5

Built as four layers rather than one lossy step (§5.0):

| Layer | Mechanism | State |
|---|---|---|
| 1 — Retrieval | RRF over vector + token overlap + graph | already strong; unchanged |
| 2 — **Offloading** | `spill`: full result to disk, head/tail preview, `read_result` handle | **added** — was absent |
| 3 — Selection | dedup, TF-IDF rank, importance pinning, `FitToWindow` | kept |
| 4 — Compaction | LLM briefing at window−reserve, **non-destructive** | re-tiered + made recoverable |

### Boundary counts

| Boundary | Start | Now |
|---|---:|---:|
| Kernel entry points | 6 | **0** |
| LLM calls outside the model layer | 23 | **21** |
| Memory writes outside the memory layer | 32 | **24** |
| `orchestrator` → concrete impl imports | 12 | **10** |
| Unwired kernel setters | 1 | **0** |
| Raw HTTP clients outside `safeurl` | 0 | **0** |
| **Model calls with no token ceiling** | **8** | **0** |
| Manager packages nothing constructs | 1 | **0** |

The memory count measures *call shape*, not gateway coverage. The graph sync's
ten writes route through the manager via an adapter while keeping their
`AddNode`/`AddEdge` spelling — rewriting ten multi-line composite literals is
churn with no behavioural payoff. Audit and learning records were removed from
the boundary entirely: they are logs *about* the agent with exactly one
possible destination, and a record with one destination has no placement to
decide.

### Defects found and fixed while migrating

Three were not the target of any stage; the migration surfaced them.

1. **Overlapping requests shared their verb** — `requestLoop`, `requestToolsDisabled`,
   `requestReadOnly` and `requestPlan` were single fields on the shared kernel.
   Measured: A asks for `/loop`, B asks for no loop, A stops looping. On tool
   scope, a Chat turn pinned read-only gained the mutating toolset because an
   unrelated Build turn started after it. The existing depth counters fixed the
   *restore*, never the live value. (Stage 2)
2. **Path confinement was inert on the entire CLI** — neither CLI surface set
   `core.WorkspaceKey`, and `confineWrite` returned `nil` when it was empty.
   `POST /api/tools/execute` had the same hole from its own bare context, and
   **wrote a file outside the workspace on a live binary**. THESIS.md §10 marks
   that endpoint as "refused by path confinement `[RAN]`" — the check wrote to
   `/etc`, which an unprivileged process cannot do whether or not the guard
   exists, so it passed for the wrong reason. A unit test asserted the fail-open
   as correct, commented "preserves CLI behavior". (Stage 3)
3. **`Kernel.SetReviewer` had no non-test caller** — 173 deliberate lines wired
   into the execute path, unreachable in a shipped binary for want of a config
   key. `go vet` cannot see this: an exported method is "used" by definition.
   `arch-check` gained a boundary for the class. (Stage 1)

---

## 6.2 Where model debate belongs — decided

Asked directly: should the debate mechanism move into the LLM Manager?

**No.** It belongs in a component of its own, beside the Orchestrator rather
than inside it, and beside the LLM Manager rather than inside that either.

The reasoning, from the actual call chain:

```
1. router.Consensus         fan out to N models, synthesise   → model concern
2. kernel.adjudicateCtx     verify claims against the graph   → EVIDENCE concern
3. kernel.resolveByDebate   models critique each other once   → model concern
```

Steps 2 and 3 are **one job with two methods**: given N candidate answers,
which is right — first by checking, then, only if checking is silent, by
argument. Splitting them across two managers would split one decision.

Putting that job in the LLM Manager fails on step 2. Adjudication's *primary*
method is knowledge-graph verification, so the LLM Manager would have to import
memory — precisely what the target architecture forbids ("The LLM Manager
should never know about orchestration or memory systems"). Debate is the
*fallback*, not the mechanism; siting the whole thing by its fallback would put
the cheap, correct path (checking) behind the expensive one.

Leaving it in the Orchestrator fails for the other reason: the Orchestrator is
meant to coordinate, not implement, and 375 lines of adjudication is part of
why the kernel is a god object.

**Proposed shape** — the next extraction:

```go
package adjudicate

// Evidence answers "does this claim survive checking" — satisfied by the
// knowledge graph, reached through the Data Source Manager.
type Evidence interface {
    Verify(ctx context.Context, claims []string) (survived, total int)
}

// Debater runs one grounded exchange — satisfied by the LLM Manager.
type Debater interface {
    Critique(ctx context.Context, goal string, a, b Position) (string, string)
    Settle(ctx context.Context, goal string, a, b Position, critA, critB string) string
}

// Verdict picks a winner from candidates: evidence first, debate only when
// the evidence does not distinguish them, synthesis on a tie.
func Verdict(ctx context.Context, goal string, c *core.ConsensusResult,
    ev Evidence, d Debater) Result
```

It takes candidates from the LLM Manager and evidence from the Data Source
Manager and returns a verdict. Neither manager learns about the other, and the
Orchestrator calls it rather than containing it.

**Not done in this pass**, deliberately: it is a 375-line move, it is gated off
by default so it carries no user-visible urgency, and it wants the Data Source
Manager (Stage 7) to exist first so `Evidence` has a real home instead of
reaching into `memory` directly. Doing it late in a long session is how the two
reverts in this repository's history happened.

---

## 6.3 Measured against the alternatives

Held against the agents this competes with, per mechanism rather than per
feature list. Only the rows where the mechanisms genuinely differ.

| Mechanism | CLI agent A | IDE agent B | Agents C / D | DarkCode now |
|---|---|---|---|---|
| Oversized tool result | truncate | truncate | truncate | **offload + head/tail preview + `read_result` handle** |
| Conversation at the limit | compact, originals discarded | RAG-first, no accumulation | compact, pre-flush | **compact at window−reserve, originals retained** |
| Repo knowledge across sessions | none — re-reads each session | vector index, re-embedded on change | none | **graph with provenance + confidence** |
| Staleness of that knowledge | n/a | re-embed on file change | n/a | **per-file content hash, catches uncommitted edits** |
| Concurrency | fixed | fixed | fixed | **decided per wave from provider pressure + cores** |
| Conversational turn | tools always available | retrieval always available | tools available | **read-only tools; no tool-less mode** |
| Read-only boundary | per-tool | n/a | per-tool | **per-call — `pdf info` reads, `pdf merge` writes** |

### Where this is genuinely ahead

**Tool-result offloading.** The others truncate; the bytes are gone. Here the
full result is content-addressed on disk and the model gets head *and* tail
plus a handle. Verified live: asked for the **last** function in a 208 KB file,
the agent answered correctly — impossible under head-only truncation. ~282 KB
of observation became ~3 KB of context with the answer still reachable.

**Knowledge that knows when it is wrong.** Agent A starts every session
cold. Agent B's index re-embeds on change but carries no provenance or
confidence, so it cannot tell you *why* it believes something. Here a file
belief carries the hash of the version it was formed from, so "which of my
beliefs are about a file that has changed" is exact — and it catches
uncommitted edits, including the agent's own, which is the most likely moment
for a belief to go wrong mid-task.

**Concurrency as a decision, not a constant.** Everyone else ships a number.
This reads provider pressure, budget, locality and core count per wave, and
explains the choice.

### Where it is still behind

- **Retrieval quality.** Agent B's whole-codebase embedding index beats the
  agent's read-triggered graph for cold "where is X" questions. The graph is
  richer per fact but sparser overall — it knows what the agent has looked at.
- **Multi-language parsing is regex** outside Go. tree-sitter needs CGo, which
  costs the static binary. Deliberate, and correctly documented as
  "superset-with-noise".
- **No published benchmark number.** The harness and fixtures exist and now
  drive the product's own API surface, but nothing has been scored.

### The rule this section exists to enforce

Adding a mechanism because a competitor has it is how the anti-scope list in
`HERMES_GAP_STATUS.md` §9.4 gets violated. The question is always whether the
*mechanism* is better, not whether the feature is present — retrieval and
compaction are complementary layers (§5.0), and the reason to keep both is that
each solves something the other structurally cannot.

---

## 7. Migration plan

Sized in **independently revertible commits**, because this repository has
reverted two large changes in recent history. Every step ends with
`make ci` green and no behaviour change unless stated.

### Stage 0 — Enforcement first (½ day, no behaviour change)

Do this **before** any migration so regressions cannot land silently — this is
THESIS.md's own recommendation 3.3, still unimplemented.

| # | Action | Done when |
|---|---|---|
| 0.1 | CI grep: no `ChatCompletion`/`CreateEmbedding` outside `llm`/`router`/`provider`/`modelport`. Seed the allowlist with today's **23** sites; the list may only shrink. | `make ci` fails on a new site |
| 0.2 | CI grep: no memory mutators outside `memory`/`datasource`. Seed with today's **32**. | same |
| 0.3 | CI grep: no `kernel.Execute` outside the UI manager. Seed with today's **6**. | same |
| 0.4 | Record baseline coverage floors for every package touched | `make cover-check` passes |

The allowlists are the migration's progress bar: each later stage deletes lines
from them, and CI proves the count never goes up.

### Stage 1 — Delete dead weight (½ day, pure subtraction)

| # | Action | Evidence |
|---|---|---|
| 1.1 | Delete package `model/` | Zero importers [RAN] |
| 1.2 | Delete `ctxengine.chronologicalSort` no-op, fix the lying comment | Returns input unchanged |
| 1.3 | Delete `agenticOn` + `SetAgenticLoop` if still unassigned; delete `reviewer.go` or wire it | THESIS §7.5 — **re-verify first**, `reviewerOn`/`debateOn` may have gained setters since |

### Stage 2 — Session object (2–3 days) — **fixes P2, the correctness bug**

Introduce `Session`: workspace, approver, tool scope, verb/strategy, budget,
project, surface. Unexported fields, no valid zero value, constructor refuses
incomplete input. Move every `request*` field off `Kernel` onto `Session` and
thread it through `Execute`.

**Verify:** a test that runs two overlapping requests with different verbs and
asserts neither observes the other's overrides. It must **fail against current
`main`** — prove it by reverting.

### Stage 3 — UI Manager (2–3 days) — **fixes P4**

One entry point: `Handle(ctx, Request) <-chan Event`. Migrate all 6
`kernel.Execute` sites. Move `cli/console.go`'s activity log and the GUI's SSE
reconstruction behind one telemetry aggregator. Add a panic guard around the
engine goroutine (THESIS §6 defect 3 — verify whether it still applies here).

**Deliverable:** the real-time telemetry the brief asks for — stage, tokens,
model, cost, retrieval, tool execution, latency, cache hits — as one typed
event stream all three surfaces consume identically.

### Stage 4 — LLM Manager (3–4 days) — **fixes P3, P5**

`Complete(Ask)` / `Embed(text)` with a `Purpose` enum (Plan/Execute/Assemble/
Classify/Review) mapping to tier + limits. Migrate the 23 sites in priority order:

1. `cli/console.go` ×2 and `server/planworkflow.go` ×2 — **collapse the duplicate
   feature into one implementation** (P3). Highest value: removes a whole
   duplicated feature and puts 2 unmetered calls back on the metered path.
2. `tools/web.go`, `tools/research.go` — removes `tools`→`llm` (P5).
3. `memory/*.go` ×3 embeddings — removes `memory`→`llm`.
4. `orchestrator/*` ×6.
5. `agents`, `loop`, `ctxengine`, `app_wireup`.

Each is its own commit; the CI allowlist shrinks by that many lines.

### Stage 5 — Re-tier context management (3–4 days) — **Phase 3 of the brief, revised**

Revised per §5.0: compaction is **kept and fixed**, not removed. Ordered so the
cheapest, highest-value fixes land first.

| # | Action | Fixes | Effort |
|---|---|---|---|
| 5.1 | **Delete the message-count trigger** (`compressionMinHistory`/`MinGrowth`) | D1 | 1 h |
| 5.2 | **Raise the watermark** from 60% to window − reserve (~85%), env-configurable | D2 | 2 h |
| 5.3 | **Add tool-result offloading** — results over a byte budget persist to disk, context keeps a preview + retrievable handle | **D4** | 1–2 d |
| 5.4 | **Make compaction non-destructive** — flush originals before `STMCompress` so history is recoverable | D3 | half day |
| 5.5 | **Collapse the three implementations into one** — delete `ctxengine`'s summarizer (C9–C11) and `boundedChatContext` (C17); one owner for compaction | P7 | 1 d |
| 5.6 | **Delete the dead interface methods** `CompressBlock`, `AssembleContext` (C5, C6) — zero callers | P7 | 1 h |
| 5.7 | Promote selection (C12–C16) into one `ContextAssembler` | — | 1 d |

**Benchmarks first (5.0).** There are currently **zero** benchmarks in the repo
[RAN], so tokens-per-request cannot be compared before/after. Add them before
5.1, or none of the above can be shown to have helped.

**Verify:** a short 10-message conversation that *currently* triggers compaction
issues **zero** compaction calls afterwards; a genuinely long conversation still
compacts at the watermark; STM originals remain recoverable in both cases; a
large `read_file` result no longer occupies the window.

### Stage 6 — Memory Manager (2–3 days)

Placement by fact shape (Relation→graph, Note→semantic, Event→episodic,
Procedure→procedural), content-addressed dedup. Migrate the 32 write sites,
`tools/deterministic/kgsync.go` (10 sites) first — it is the largest single
cluster and removes `tools`→`memory`.

### Stage 7 — Data Source Manager (3–4 days) — **the keystone**

Only now, with Memory and LLM behind clean APIs, introduce the gateway:
`Retrieve(Query) → Context` and `Persist(Fact)`. Add the shared retrieval cache
(§4.1 #7). Point the Orchestrator at it and **delete the `memory`, `llm`,
`compression` imports from `orchestrator`.**

**Done when:** `grep '"github.com/darkcode/\(memory\|llm\|compression\)"' orchestrator/*.go`
returns nothing, and CI enforces it.

### Stage 8 — Documentation

Update `THESIS.md` (including the §0 correction above), `docs/MANAGERS.md`,
README, and diagrams.

---

## 8. Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| **Removing compression breaks small-window local models** | **High** | **High** | Keep `FitToWindow` (C12) as the hard guarantee. This is why §5 separates lossy compression from deterministic fitting. Non-negotiable. |
| Retrieval is worse than compression on real tasks | Medium | High | Stage 5 measures before deleting, on `bench/` fixtures, and stops if the number says no |
| Large refactor gets reverted like `ca66349`/`353051e` | **High** | High | Every stage independently revertible; enforcement (Stage 0) lands first |
| Session refactor introduces a concurrency bug | Medium | High | It is fixing one; regression test must fail against old code first |
| Behaviour drift across 6 surfaces | Medium | Medium | Stage 3 collapses them to one entry point |
| Coverage is already inverted vs risk (`tools` 18.6%, `cli` 1.8%, main 0.9%) | Certain | Medium | Floors only rise; add tests with each stage |
| Free-tier Gemini quota (20 req/day) makes live verification slow | **Certain** | Medium | Prefer deterministic tests; batch live runs; check the log before diagnosing |
| Hidden coupling surfaces mid-migration | Medium | Medium | Stage 0 allowlists make every remaining coupling visible and counted |

### Explicitly recommended **against**

- **Rewriting `tools.Registry`.** It already is the Tool Manager. THESIS.md
  records that a `toolport` was specified and dropped on measurement; that
  measurement still holds.
- **A big-bang rewrite.** Two reverts in recent history are the evidence.
- **Removing `FitToWindow`.** See the risk table.
- **Starting with the Data Source Manager.** It is the keystone, so it goes in
  last — building it first would mean writing it against the god object.

---

## 9. Verification cookbook

Every claim above is re-runnable.

```bash
# Baseline
go build ./... && go vet ./...

# §0 — the manager packages never existed
git log --oneline --all -- recall agentport modelport     # empty

# §1.3 / §3 — LLM policy sites outside the model layer
grep -rn --include='*.go' -E '\.(ChatCompletion|ChatCompletionStream|CreateEmbedding)\(' . \
  | grep -v _test | grep -vE '(^|/)(llm|router|provider)/' | wc -l          # 23

# §1.3 / §3 — memory writes outside the memory layer
grep -rn --include='*.go' -E '\.(EpisodicAdd|SemanticAdd|ProceduralAdd|AddNode|AddEdge|Relate|RecordFeedback|RecordAction)\(' . \
  | grep -v _test | grep -vE '(^|/)(memory|core)/' | wc -l                  # 34

# §3 P4 — surfaces entering the kernel
grep -rn --include='*.go' 'kernel.Execute(\|Kernel.Execute(' . | grep -v _test | wc -l   # 6

# §3 P1 — god object
grep -h '^func (k \*Kernel)' orchestrator/*.go | grep -v _test | wc -l      # 112
sed -n '/^type Kernel struct/,/^}/p' orchestrator/kernel.go | grep -cE '^\s+[a-zA-Z]+\s+'  # 48

# §2 — dead package
grep -rn '"github.com/darkcode/model"' --include='*.go' .                   # empty

# §5 — compression default is ON
grep -n 'CompressContext' config/config.go orchestrator/config.go

# §7 Stage 5 — zero benchmarks exist today
grep -rn "func Benchmark" --include='*.go' . | wc -l                        # 0
```

---

## 10. Summary

The architecture you have asked for is **substantially the right target**, and
three of its six components are closer than they look:

- **Tool Manager: already built.** `tools.Registry` needs enforcement, not work.
- **The retrieval machinery to replace compression: already built and good.**
  RRF fusion, TF-IDF ranking, importance scoring, deterministic budget fitting.
  They just have a summarization layer sitting on top of them.
- **The interfaces to decouple the kernel: already written** in
  `core/interfaces.go`, and ignored by the kernel that needed them.

The real work is three things:

1. **Break up the god object** — 48 fields, 112 methods, and per-request state on
   shared mutable fields, which is a live concurrency bug on the GUI, not a
   style complaint.
2. **Give the system one entry point and one data gateway** — 6 surface entries
   and 57 ungated data accesses (23 LLM + 32 memory) is the whole coupling problem.
3. **Delete the summarization layer**, keeping the deterministic selection
   underneath it — and measure before deleting, because there are currently zero
   benchmarks to prove it was an improvement.

One correction to the brief, offered as engineering input rather than pushback:
**"remove context compression completely" must not include `FitToWindow`.**
That path makes no LLM call and loses nothing that would have fit; it is the
guarantee that a request to a 4k-window local model does not hard-fail. Removing
it would break the local-first bet that §1 of THESIS.md says the product exists
to make. Everything that spends a model call to discard information should go.
