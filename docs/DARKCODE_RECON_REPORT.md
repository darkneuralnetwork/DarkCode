# DarkCode — Architectural Reconnaissance Report

> **Note:** this document predates the `kernel/infra/surfaces/model` directory
> restructure (August 2026). File paths it cites reflect the flat layout at
> the time of writing; the content and findings are otherwise unchanged.


**Method:** read-only investigation of the working tree as checked out (no `.git` present, so
history is unavailable). Every claim below is marked **FACT** (cites a file, often a line),
**INFERENCE** (a reasonable interpretation not directly stated in code), or **UNKNOWN**
(insufficient evidence). Marketing language from `README.md` was treated throughout as an
unverified claim to check against `.go` source — never as evidence in itself; a running
"documented claim" table appears in each subsystem section and is consolidated in §17.

This report was compiled from five independent subsystem investigations (kept in full at
`docs/recon/*.md` — orchestrator, memory, tools/permission/security, interfaces, model layer)
plus direct verification of their highest-stakes claims: several numeric and structural claims
in an earlier draft of this report were re-checked line-by-line against source during
consolidation, and three were found and corrected — an "empty `concurrency/` package" claim (§8),
a mis-attributed `memory-writes` breakdown (§5), and an undercounted test-function total (§12); see
the closing note at the end of this report for the full list.

No application code was modified during this investigation (**FACT**, verified: `find . -name
'*.go' -not -path './build/*' -mmin -180` returns nothing at time of writing, and spot-checked
core files show mtimes from two days prior to this session). One new file was created (this
report) and five reference files under `docs/recon/`; the prior in-progress audit doc,
`DARKCODE_ARCHITECTURE_AUDIT.md`, was left untouched.

---

## 1. Executive Summary

DarkCode is a single-binary, local-first autonomous coding-agent platform written in Go
(module `github.com/darkcode`, Go 1.25/1.26, **~68,000 lines of Go across 299 non-test files,
plus ~28,000 lines of test code across 205 test files** — a ~41%-by-LOC test-to-source ratio,
directly counted). It is architecturally a from-scratch competitor to tools like Claude Code:
one process serves four surfaces — an interactive CLI (readline REPL), a web GUI (embedded
static frontend + JSON/SSE API), an editor integration over the Agent Client Protocol (Zed/VS
Code/JetBrains), and an OpenAI-compatible `/v1/chat/completions` endpoint — all funneling into
one orchestration kernel (**FACT**, `uiport/uiport.go`).

The codebase is unusually well-documented at the source level: nearly every non-trivial function
carries a comment explaining *why*, frequently naming a specific historical defect the code exists
to prevent (e.g., "CLI surfaces never set the workspace, so path confinement was inert"; "173 lines
wired into the execute path were unreachable in a shipped binary"). These are treated below as
FACTs about the codebase's own documented history, not embellishment — each is independently
corroborated by the code that now guards against the described failure. Git history itself is
unavailable in this checkout (no `.git`), so these are source-comment archaeology, not commit-log
verification — see §16.

Four properties stood out as genuinely more sophisticated than the README's marketing framing
suggests: (1) the "cognition cascade" is a real **online-calibration system** with per-rung and
per-fact confidence thresholds that self-adjust from re-ask detection, not a static ladder; (2) the
**local-model resource governor** does real capacity planning (weights + KV cache + LoRA + overhead
vs. free memory, plus a GPU `-ngl` layer-count estimate) and degrades context before quality; (3)
the **prompt-injection and secret scanners** are real, specific regex- and Unicode-based detectors,
not aspirational claims; (4) the **architecture-boundary CI gate** (`scripts/arch-check.sh` +
`.arch-baseline`) enforces eight layering rules as ratcheting ceilings, not prose conventions.

Four genuine defects were found — each confirmed by direct code-path reading (a static pass; none
were executed end-to-end against a running process) — and are detailed in §14:
- **[Highest confidence, highest severity]** `darkcode --acp` on a freshly-installed, unconfigured
  system — precisely the README's own quick-start scenario for editor integration — runs an
  interactive setup wizard that prints to stdout and blocks on terminal input, because `main.go`'s
  config-validation branch checks `!uiMode && !guiFlag` but never checks the ACP flag. This breaks
  the JSON-RPC handshake the editor is waiting on.
- Under Relaxed safety level, a non-central tool call's arguments are approved **before** the
  secret scanner ever runs, so a credential-shaped string in a tool argument is never scanned or
  flagged at that safety level.
- Three per-request settings — routing mode, safety level, and brain/local-preference selection —
  remain last-writer-wins across genuinely concurrent overlapping requests; this is
  *self-documented* in the code as a known, not-yet-fixed gap, unlike the four sibling settings
  (Loop, Tools-disabled, Read-only, Plan-forced) already fixed the same way.
- The legacy (still-wired) memory store matches entries to mutate/delete by **substring**, not by
  ID — two entries sharing common text could have the wrong one mutated or deleted.

## 2. Architecture

DarkCode is organized as roughly six layers, matching `main.go`'s own printed help text
(**FACT**, `main.go:349-355`) reasonably closely once verified against the dependency graph:

1. **Interfaces** (`server/`, `cli/`, `ui/`, `acp/`) — four surfaces, one shared entry contract.
2. **uiport** (`uiport/`) — the single mandatory-workspace gateway from any surface into the kernel.
3. **Orchestration Kernel** (`orchestrator/` + `loop/`, `dag/`, `plan/`, `ctxengine/`,
   `adjudicate/`, `datasource/`, `concurrency/`) — planning, the cognition cascade, DAG/ReAct
   execution, consensus, and per-wave concurrency sizing.
4. **Model layer** (`router/`, `llm/`, `provider/`, `provider/embedded/`, `modelport/`,
   `capability/`, `compression/`) — one generic OpenAI-shaped HTTP client reused across every
   cloud provider's OpenAI-compatible endpoint, plus a real local llama.cpp subprocess lifecycle.
5. **Tool/Security layer** (`tools/`, `permission/`, `security/`, `safeurl/`) — tool dispatch,
   the approval gate, sandboxing, SSRF/air-gap guarding, content-safety scanning.
6. **Memory** (`memory/`, `recall/`) — four memory tiers plus a structural knowledge graph, all
   JSON-file-backed with a debounced, crash-safe (temp+fsync+rename) writer.

Layering is not merely conventional: `.arch-baseline` (**FACT**, repo root) declares eight
boundary metrics as CI-enforced ceilings, checked by `scripts/arch-check.sh`. Six are currently at
**zero violations** (`kernel-entry`, `orchestrator-impl-imports`, `raw-http-clients`,
`unwired-managers`, `unbounded-completions`, `unwired-kernel-setters`), and two have real,
enumerable current violations (`llm-calls: 13`, `memory-writes: 24`) — confirmed by directly
running `scripts/arch-check.sh --list`, which prints every offending file:line (full table in §7).

## 3. Component Map

```
                         ┌─────────────────────────────────────────┐
                         │              4 SURFACES                  │
                         │  cli/  server/(+web/)  acp/  (OpenAI /v1) │
                         └──────────────────┬────────────────────────┘
                                             │  uiport.Manager.Execute
                                             │  (mandatory workspace; the
                                             │   ONLY sanctioned path in)
                                             ▼
                         ┌─────────────────────────────────────────┐
                         │        ORCHESTRATION KERNEL               │
                         │  orchestrator/ (cascade, DAG, consensus,  │
                         │  plan-gate, reviewer, memory_recorder)    │
                         │  + loop/ dag/ plan/ ctxengine/ adjudicate/│
                         │  + concurrency/ (per-wave sizing)         │
                         └───┬──────────┬──────────┬────────────┬───┘
                              │          │          │            │
                     router/  │   tools/ │  memory/ │  permission/
                     llm/     │  security/ recall/  │  security/
                     provider/│  safeurl/ │          │
                     ▼        ▼          ▼            ▼
              [cloud/local  [shell,file, [4 memory   [approve/deny,
               model calls]  git, web,    tiers +     sandbox, blast-
                              MCP, LSP,    knowledge   radius escalate]
                              debug tools] graph]
```
(**FACT** for every edge shown — each is a real import in the `go list` dependency graph gathered
during this investigation; ASCII layout is this report's own summary, not from source.)

## 4. Runtime Flow

One user turn, traced end-to-end (**FACT**, file:line citations from `orchestrator/kernel_execute.go`,
detailed in full in `docs/recon/orchestrator.md`): cost-governor check → STM append →
pending-plan-approval gate → **cognition cascade** (deterministic tools → answer cache → knowledge
graph → episodic recall, all before any LLM call) → project-context injection → conditional
token-window-based compression → complexity assessment → clarification gate → chat/general fast
paths → **either** the agentic ReAct loop (plan-first, contract-bound) **or** trivial-direct
execution **or** deep planning → optional plan-approval gate (blast-radius-aware) → DAG execution
→ result merge (best-effort even on partial cancellation) → output verification → failed-
acceptance repair (handed back to the loop) → outcome recording (episodic + learning + audit + KG
+ skill extraction) → emit.

Every model call inside this flow routes through `router.Route`, which applies a task-type local-
offload intercept first, then dispatches on routing mode (single/escalation/consensus), and — if
force-local is set — refuses explicitly rather than silently reaching the cloud (**FACT**,
`router/router.go:365-372`: `"force-local mode is active but no local model is available"`).

**A second full trace, entry to kernel, read directly through the handler body**
(`server/openai_compat.go`, `handleOpenAIChat`): `POST /v1/chat/completions` → method check
→ decode OpenAI-shaped JSON body (capped by `http.MaxBytesReader`) → walk `Messages` backward to
take only the newest `role:"user"` entry as the prompt (earlier turns are deliberately dropped —
the kernel owns conversation state, so replaying history would double-count it) →
`metrics.Default.RecordTurn()` → `s.port.Execute(r.Context(),
uiport.Request{Query: prompt, Surface: SurfaceAPI, Workspace: s.ActiveWorkspace()})` — the same
`uiport.Manager.Execute` documented above, confirming this surface takes no shortcut around it →
on success, either single-shot OpenAI-shaped JSON (`"chatcmpl-<ns-timestamp>"`, `finish_reason:
"stop"`) or `streamOpenAIChat` for `req.Stream == true`.

**A third trace, the one that surfaced this report's highest-severity finding**: `darkcode --acp`
→ `main.go` parses flags → `cfg.Validate()` (`config/config.go:726`) → on a fresh install with no
`api_key` and `EnableLocalLLM` unset, this **fails** → the guard at `main.go:139` is
`if !uiMode && !guiFlag` — **`acpFlag` is not part of the condition** → `config.RunInteractiveSetup`
runs → `config/setup.go:25` calls `fmt.Println` unconditionally (no TTY check precedes it) →
stdout now carries human-readable wizard text instead of JSON-RPC, and the wizard blocks on
`readline` waiting for terminal input that an editor's subprocess pipe will never supply. See §14.1.

## 5. Data Flow

- **User input** → STM (in-memory, capped at 50 messages, `memory/system.go:116`) → optionally
  compressed into a briefing when the token window fills.
- **Tool observations** (file reads, command output) → `recall.Manager.Remember` → the shape-typed
  destination store (KG node/edge, semantic note, episodic event, procedural skill) — content-
  addressed so re-observing the same fact overwrites rather than duplicates (**FACT**,
  `recall/recall.go:26-30, 92-141`).
- **Outcome of a turn** → `orchestrator/memory_recorder.go` writes episodic memory, learning
  feedback, an audit-log entry, and several KG node/edge types (task, fix, decision, file, tool,
  agent). Of the 24 currently-tracked `memory-writes` boundary entries (outside memory/core/recall,
  per `.arch-baseline`), 12 are in `orchestrator/memory_recorder.go` and 10 in
  `tools/deterministic/kgsync.go` (directly counted via `scripts/arch-check.sh --list`), with 1
  each in `ingest/ingest.go` and `tools/memory_tool.go`. **Important nuance, carried over from the
  script's own header comment** (`scripts/arch-check.sh:60-79`, read directly): this metric counts
  *call shape*, not actual gateway bypass — "as of this writing none of the 24 does" bypass the
  gateway. The 22 graph writes go through `core.KnowledgeGraphStore` handles that resolve to
  `recall.GraphWriter` at runtime (`orchestrator/kernel.go`'s `graph()` and the `deterministicKG`
  handed out in `app_wireup.go` both distribute the *manager's* writer, not a raw store); the
  remaining 2 are the already-gated fallback arm of a write that goes through `recall` whenever
  a gateway is installed, which it is in the real binary. Treat this as tracked call-shape debt to
  keep an eye on, not 24 confirmed holes in the memory gateway.
- **Persistence**: every durable store is a flat JSON file under `~/.darkcode/memory/` (system-
  wide, shared across projects), written via a debounced (2s) writer that flushes through
  temp-file → `Sync()` → `Rename()` (**FACT**, `memory/writer.go:139-163`) — genuinely crash-safe.
  One exception: a **legacy, non-atomic `memory.json` store** (`memory/store.go`, plain
  `os.WriteFile`, no fsync/rename) is still constructed and wired at startup for tool backward-
  compatibility (**FACT**, `app_wireup.go:153-156, 172-175`) and represents a second, weaker
  persistence path living alongside the real one — see §14.3.

## 6. Domain Model

Core shared vocabulary lives in `core/` (fan-in 27, the highest of any package — **FACT**, `go
list` dependency graph), including `core.Message`, `core.EpisodicEntry`, `core.Skill`,
`core.KGNode`/`core.KGEdge`/`core.KGRelationType`, `core.SubAgentResult`, `core.ModelTier`,
`core.RoutingMode`. The knowledge graph is the closest thing to a structural "domain model" beyond
chat: nodes represent files, symbols, tasks, decisions, fixes, tools, and agents; edges carry
typed relations (`KGRelProducedBy`, `KGRelFixedBy`, `KGRelDecidedBecause`, `KGRelContains`,
`KGRelUsedBy`) plus a weight and provenance (**FACT**, `orchestrator/memory_recorder.go:290-395`,
`recall/recall.go:59-73`). A `plan.Graph` (task decomposition with acceptance criteria) is the
domain model for "what the agent is doing right now"; it is distinct from the KG.

## 7. Data and Business Invariants

Selected invariants, each classified by whether it is **enforced** (code structurally prevents
violation), **assumed** (code relies on it but does not check it), **tested**, or **untested**.

| Invariant | Location | Enforced? | Tested? | What could break it |
|---|---|---|---|---|
| Every request into the kernel carries a non-empty `Workspace` (path confinement cannot work without one) | `uiport/uiport.go:113-121` (`Request.Validate`) | **Enforced** — `Execute` refuses the request outright | **Tested** (`uiport/uiport_test.go`) | A future 5th surface that builds `uiport.Request{}` without setting `Workspace` — the type system doesn't prevent this, only the runtime check does |
| A policy file can only *restrict* config, never grant beyond it | `config/policy.go:162-186` (`Policy.Apply`) — every field only tightens (`min`/`max` comparisons, one-directional append for deny rules) | **Enforced** by construction — no code path in `Apply` widens a setting | **Tested** (`config/policy_test.go`) | A new `Policy` field added later without the same one-directional discipline |
| An unanswered approval prompt denies rather than hangs | `permission/gate.go:543-552` (`askWithTimeout`) — literal `Unanswered: true`, feedback string `"denied (fail closed)"` | **Enforced** | **Tested** (`permission/timeout_test.go`) | None found; the design is sound and covered |
| Deny rules refuse a call before the relaxed fast-path, session cache, or approver can approve it | `permission/gate.go:384-402` (checked ahead of `LevelRelaxed`'s fast return at line 412) | **Enforced** | Present per test-file listing (`permission/lock_tests_test.go` and related) — exact regression coverage of this *ordering* specifically not individually confirmed | A future refactor that reorders `Check()`'s body without re-reading the doc comment explaining why the order matters |
| Every tool call's arguments are secret-scanned before approval | `permission/gate.go:432` (`argsContainSecret`) | **Assumed, violated for one configuration**: under `LevelRelaxed` and a non-central call, `Check()` returns at line 412-415 — before line 432 ever runs. `gate.go:50` documents Relaxed as "auto-approve everything" by design, so this is a real gap in the *unqualified* README claim ("credentials in tool args force a prompt") for a user-selectable level, not a bug in an internal invariant the code claims to hold universally | **Untested** for this specific ordering interaction (no test found asserting a secret is still caught under Relaxed) | Exactly the configuration described: Relaxed safety + a non-blast-radius-central file |
| A checkpoint snapshot is taken before every *mutating* tool call, never before a read-only one | `tools/registry.go:287, 341-356` (`r.snapshot(mutatingTool(r, calls))`, gated `ckpt.Snapshot`) | **Enforced** — gated on the mutating-tool check, and a snapshot failure is logged, never blocks the tool | Present (registry-level tests exist; exact snapshot-gating assertion not individually opened) | A new tool that mutates the filesystem but isn't classified as "mutating" by `mutatingTool` |
| Memory writes are atomic — a reader never observes a truncated file | `memory/writer.go:139-163` (`atomicWriteFile`: temp file → `Sync()` → `Close()` → `Chmod` → `Rename()`) | **Enforced** for the current `System`'s four tiers + KG | Design is sound and (per file naming) `memory/writer_align_test.go` covers at least the related 32-bit-atomics regression; no test found that simulates an interrupted write and checks for a leftover `.tmp-*` file | **Violated by construction** for the coexisting legacy `memory/store.go` path (§14.3) — that store is plain `os.WriteFile`, no temp/fsync/rename |
| Nothing but the ACP protocol writes to stdout while running under `--acp` | Asserted only in a comment, `app_acp.go:39` ("nothing else may write to it") | **Assumed, not enforced anywhere** — no build tag, lint rule, or runtime guard prevents another package from calling `fmt.Println`/`fmt.Print`/`log.Print*` to stdout | **Untested** | Confirmed broken in the specific first-run scenario in §14.1: `config.RunInteractiveSetup` violates this invariant directly and unconditionally |
| Mode / safety-level / brain(local-preference) request overrides are isolated per concurrent request | `orchestrator/request_state.go:1-32` (own comment) | **Partially enforced**: Loop/Tools-disabled/Read-only/Plan-forced ARE isolated via `context.Context` (`request_state.go:34-141`). Mode, Safety, and Brain are **not** — they mutate shared router/gate fields via a save/depth-counter/restore mechanism that the file's own comment says remains "last-writer-wins while requests overlap" | **Tested for the fixed half** (`orchestrator/override_isolation_test.go`, `override_scope_test.go`) — no test found exercising the still-open half under genuine concurrency | Two simultaneous turns setting different safety levels; see §14.5 |
| A forbidden model can never be reached by any routing path once a policy denies it | `config/policy.go:212-260` (`Policy.ModelAllowed`) — checked before a model is registered with the router | **Enforced at registration time** (a denied model is simply never added to `Router`'s maps) | Present (`config/policy_test.go`) | Any future code path that constructs an `llm.Client` directly from `config.ModelConfig` without going through the policy-gated registration in `app_wireup.go` |
| Every completion request sets a bounded `MaxTokens` | `.arch-baseline`'s `unbounded-completions` = 0, CI-enforced via `scripts/arch-check.sh` | **Enforced by CI gate**, not by a type-level guarantee | Enforced = effectively tested on every CI run | A new call site added without `MaxTokens` would fail CI before merge, per the ratchet design — but nothing stops a local `go build`/manual run from shipping it if CI is bypassed |

## 8. Dependency Graph

Full package-level import graph gathered via `go list -f '{{.ImportPath}} -> {{join .Imports " "}}'
./...`, filtered to internal (`github.com/darkcode/*`) edges — 54 packages, every edge a FACT.

**Highest fan-in** (most depended upon — the shared vocabulary/utility layer):
`core` (27), `safeurl` (11), `internal/strutil` (11), `memory` (10), `modelport`/`observability`/
`ui` (9 each), `config`/`llm`/`tools` (8 each), `router` (7).

**Highest fan-out** (the wiring/integration layer, expected to be large):
root `main`/`app_*.go` (33), `server` (26), `cli` (23), `orchestrator` (21), `tools` (20).

**Enforced boundaries** (`.arch-baseline`, all eight metrics, current state via
`scripts/arch-check.sh --list`, run directly during this pass):

| Boundary | Ceiling | Current | Enforced? |
|---|---|---|---|
| `kernel-entry` (only uiport reaches the kernel) | 0 | 0 | Fully enforced |
| `orchestrator-impl-imports` | 0 | 0 | Fully enforced |
| `raw-http-clients` (all HTTP via safeurl/) | 0 | 0 | Fully enforced |
| `unwired-managers` | 0 | 0 | Fully enforced |
| `unbounded-completions` (every model call sets MaxTokens) | 0 | 0 | Fully enforced |
| `unwired-kernel-setters` | 0 | 0 | Fully enforced (this is the exact metric that would have caught the historical `SetReviewer` bug, §1) |
| `llm-calls` outside llm/router/provider/modelport | 13 | 13 (ratcheted from a documented historical 23, per `scripts/arch-check.sh`'s own header comment) | Partially — 13 known, tracked, allowed sites |
| `memory-writes` outside memory/core/recall | 24 | 24 (ratcheted from a documented historical 34) | Partially — 24 known call-shape sites, 12 in one file (`orchestrator/memory_recorder.go`), 10 in `tools/deterministic/kgsync.go`; see §5's caveat that none currently bypass the gateway |

**Suspicious/notable relationships**: `concurrency/` (118 lines + a 116-line test file — **FACT**,
directly read, correcting an earlier misreading during this report's drafting that treated its
zero *internal-package* imports in `go list` output as an empty package) is a small, genuinely
wired computation: `concurrency.Decide(Signals) Decision` sizes per-wave DAG parallelism from
ready-task count, CPU cores, and live model rate-limit pressure; `orchestrator/concurrency.go`'s
`resolveConcurrency` calls it and applies the result via `k.executor.SetMaxConcurrent(d.Limit)`
before each wave. Its own header comment documents that this used to be dead code (a
`SetMaxConcurrent` call with no non-test caller) and was fixed — consistent with the project's
general pattern of ratcheting out exactly this kind of drift. `orchestrator` does not import
`memory` or `llm` directly, consistent with the enforced boundary — it reaches memory via the
`core.MemoryStore` interface and model calls via `router`/`modelport` (**FACT**, confirmed both by
the absence of those imports in the dependency graph and by the CI-enforced zero on
`orchestrator-impl-imports`).

## 9. Persistence

No database anywhere in the codebase (**FACT** — confirmed by dependency-graph inspection; every
store found is a flat JSON file). Files, all under `~/.darkcode/<name>/` by default (system-wide,
shared across projects — **FACT**, `app_wireup.go:99-108`, `defaultDarkcodeDir`):
`memory.json` (legacy, non-atomic), `episodic.json`, `semantic.json`, `procedural.json`,
`architecture.json`, `health_history.json`, `cascade_log.jsonl`, `model_reliability.json`,
`config.json`. Checkpoints are a **content-addressed blob store** (`checkpoint/`, SHA-256-keyed
per `checkpoint.go`'s `crypto/sha256` import) taken automatically before every mutating tool call —
confirmed at the exact call site: `tools/registry.go:355` (`ckpt.Snapshot(tool, "before "+tool)`),
gated by `r.snapshot(mutatingTool(r, calls))` (`registry.go:287`) so read-only tools genuinely
never trigger a snapshot (**FACT** — directly verifies the README's "before every file-modifying
action" claim).

## 10. External Integrations

- **LLM providers**: OpenAI, Anthropic, OpenRouter, Google, Groq, DeepSeek, Mistral, xAI,
  Together, Ollama, LM Studio, plus an embedded local engine — all reachable (**FACT**,
  `config/providers.go`), but via **one generic client** speaking the OpenAI `/chat/completions`
  wire format to each provider's own OpenAI-compatibility endpoint (every catalogued base URL is
  an OpenAI-compat surface, including Anthropic's `api.anthropic.com/v1/chat/completions` and
  Google's `generativelanguage.googleapis.com/v1beta/openai`), with provider-specific auth-header
  patches layered on top (Anthropic: `x-api-key` + `anthropic-version`; Google:
  `x-goog-api-key`) — **FACT**, `llm/client.go:140-163`, `config/providers.go`.
- **Local model**: a real `llama-server` (llama.cpp's OpenAI-compatible HTTP server) subprocess —
  not an in-process/CGO binding (`go.mod` has zero cgo dependencies; build is `CGO_ENABLED=0`) —
  auto-downloaded, spawned, and load-planned by a genuine resource governor: a per-architecture
  KV-cache table budgeted against 55% of free RAM (`provider/embedded/governor.go`), plus a
  VRAM-based GPU layer-count (`-ngl`) estimate (`embedded_stub.go:500-536`, `~40MB/layer` for
  q4_k_m quantization, capped at 90% of VRAM) passed to llama-server's launch args
  (`manager.go:223-224`) — **FACT**.
- **Web/research**: `safeurl`-guarded HTTP client for `web_search`/`web_fetch`/`research` tools,
  with dial-time SSRF and air-gap enforcement that specifically defeats DNS-rebinding TOCTOU
  attacks via a `net.Dialer.Control` hook that re-validates the actually-resolved IP at connect
  time, not just at URL-parse time — **FACT**, `safeurl/safeurl.go:68-96` (read in full).
- **GitHub**: a dedicated tool (`tools/github.go`).
- **MCP**: both directions confirmed real — client (`tools/mcp_client.go`, connects external MCP
  tool servers over stdio/HTTP) and server (`server/mcp.go`, JSON-RPC 2.0 over HTTP at
  `/api/mcp`) — **FACT**.
- **Language servers**: `intelligence/lsp.go` is a **real, hand-rolled LSP client** — JSON-RPC 2.0
  over stdio with LSP's Content-Length framing, no external dependency (`lsp.go:15-16`); a real
  per-language server table (`serverCommands`, `lsp.go:46-50`: `gopls` for Go,
  `typescript-language-server`/`vtsls` for TS, `pyright-langserver`/`pylsp`/`jedi-language-server`
  for Python, `rust-analyzer` for Rust — first found on `PATH` wins); requests are bounded
  (`lspRequestTimeout = 10s`) and every failure falls back to the AST index rather than erroring —
  **FACT** (upgraded from INFERENCE after direct file read).
- **Debugger**: `debugger/delve.go` — a real Delve integration over `net/rpc/jsonrpc` (Go's
  standard library speaks Delve's headless JSON-RPC protocol, no external dependency needed); the
  package always builds-and-attaches through `dlv debug`/`dlv test` rather than attaching to a
  pre-built binary, specifically because breakpoints silently fail to bind against an
  already-optimized build (`delve.go:1-17`, read in full) — **FACT** (upgraded from INFERENCE).
  Python/JS debugging goes through the Debug Adapter Protocol behind the same tool (`debugger/dap.go`,
  818 lines — file presence and size confirmed, internals not read in this pass).
- **Editors**: real Agent Client Protocol server, correct NDJSON-RPC-2.0-over-stdio framing
  (explicitly distinguished in its own doc comment from LSP/DAP's Content-Length framing),
  including a genuine outbound `session/request_permission` round trip for approvals
  (`acp/permission.go:73`) — **FACT**, `acp/acp.go`. The README's more specific claim of having
  been "verified against Zed's official client SDK" has no corroborating conformance test in this
  tree — **unverified**, not contradicted.

## 11. Background Processing

- **Health daemon** (`memory/kg_daemon.go`): watches the knowledge graph on a schedule, genuinely
  self-throttled to a configurable share of one CPU core (default 5%, `DefaultCPUPercent = 5`,
  clamped 1-50) by measuring scan time and sleeping `elapsed*(100-p)/p` (`kg_daemon.go:170-188`,
  read in full — a scan costing 1s at 5% buys 19s of rest; a repo 10x larger scans 10x less often
  rather than costing 10x the CPU), with alerts firing only on state transitions (a cycle present
  for a year stays silent) — **FACT**.
- **DAG execution**: wave-based, concurrent sub-task execution sized by `concurrency.Decide`
  (§8); a real resume-skip-completed-tasks path exists (`orchestrator/dag_executor.go:135-143`,
  `NewExecJournal(runsDir, goal).Resumable()` checked at the start of `executeDAG`, logs "Resuming:
  N task(s) already completed") — **FACT** that the mechanism exists and is invoked. Whether a
  genuine process kill mid-run produces a resumable journal on next start was **not independently
  exercised** (no dedicated test found simulating a process kill) — **UNKNOWN**, flagged for
  follow-up; do not read the wiring as proof of the end-to-end crash-resume claim.
- **Cognition-cascade calibration**: not a scheduled job, but an always-on online-learning loop —
  every turn's cascade decision is logged, and per-rung thresholds self-raise (never lower) when a
  rung's re-ask rate exceeds 30% over ≥5 samples (`orchestrator/cascade.go:280-294`) — **FACT**.
  Thresholds live only on the in-memory `Kernel` struct; no load-from-log code was found that would
  restore calibration across a process restart — **INFERENCE that this resets on restart**, not
  confirmed by an explicit "no such code" search of every reader of `cascadeLogPath`.
- **Async embedding backfill**: embeddings computed off the write path, tracked via a
  `sync.WaitGroup` so shutdown waits for in-flight vectors rather than losing them — **FACT**,
  `memory/system.go:100-105`.

## 12. Testing Architecture

- **205 test files, ~27,962 lines**, against **299 non-test files, ~67,932 lines** — a ~41.2%
  test-to-source ratio by LOC (**FACT**, direct `wc -l` count this pass, matching the figures
  cited in §1 to the line).
- **Only two packages have zero test files**: `ui/` (1 source file, 357 lines — real
  subscriber/history-ring event-bus logic, not pure styling) and `cli/tui/` (exactly 2 files,
  `input.go` + `selector.go` — small bubbletea widgets, confirmed via direct directory listing;
  `go list` shows no darkcode-internal imports for this package, i.e. pure rendering over an
  external UI library) — **FACT**, narrowing the prior in-progress audit's same finding.
- **Heaviest-tested subsystems**: `tools/`+`permission/`+`security/` combined have **154** `func
  Test...` definitions (directly counted via grep this pass), the highest concentration in the
  repo — consistent with being the highest-risk subsystem. `server/` has 16+ dedicated test files
  spanning chat, config, idempotency, middleware, pagination, SSE, and web routing. `orchestrator/`
  separately tests kernel-execute, DAG-executor, consensus, cascade, plan-gate, and reflection
  (34 test files against 26 source files — an unusually high test-to-source *file* ratio even by
  this codebase's own standard). `memory/` has a matching `_test.go` for nearly every `kg_*.go`
  file, plus dedicated concurrency (`system_concurrency_test.go`) and retrieval-determinism tests.
- **Directly re-run this pass, not merely corroborated from the prior audit**: `go build` (clean,
  exit 0), `go vet ./...` (clean, exit 0), `go test ./...` (46 packages `ok`, 3 report `[no test
  files]` — root, `ui`, `cli/tui` — exactly matching the prior audit's baseline split), and
  `go test -race ./...` (also clean, all 46 packages `ok`, zero races reported). **Not** re-run
  this pass: `make ci`'s `arch-check`/`leak-check` stages specifically as a combined pipeline
  (though `scripts/arch-check.sh --list` was run directly and its output is cited throughout this
  report) and `make bench`.
- **Likely-weak spots** (not confirmed broken, flagged as plausible gaps): the still-open half of
  the mode/safety/brain concurrency isolation (§14.5) has no test exercising genuinely concurrent
  overlapping requests with different values, only the already-fixed half does; crash-mid-write
  recovery for the atomic writer (design is sound, no test found that simulates an interrupted
  write and checks for a leftover `.tmp-*` file); DAG crash-resume specifically across a real
  process restart (§11); the Relaxed-safety secret-scanning gap (§14.2) has no regression test
  either confirming or denying the current behavior.

## 13. Critical Files

| Path | Purpose | Why it matters |
|---|---|---|
| `uiport/uiport.go` | Single mandatory-workspace entry contract from any surface into the kernel | Documents and fixes a real historical security bug (inert path confinement on CLI); the whole four-surface architecture hinges on this one file |
| `orchestrator/kernel_execute.go` | `Kernel.Execute` — the full per-turn control flow | The spine of the entire system; every feature (cascade, planning, DAG, loop, consensus) is wired here |
| `orchestrator/cascade.go` | Self-calibrating cognition cascade | The system's core cost-saving mechanism, and more sophisticated (online-learning) than documented |
| `orchestrator/request_state.go` | Per-request override isolation via `context.Context` | Documents a real historical concurrency bug and its partial fix; also where the still-open gap (§14.5) lives |
| `permission/gate.go` | Approval decision chain | The security spine; also where this report's Relaxed-mode secret-scanning gap lives (§14.2) |
| `main.go` | Flag parsing, mode selection, config-validation branch | Also where the ACP first-run bug (§14.1) lives — `if !uiMode && !guiFlag` at line 139 |
| `config/setup.go` | Interactive first-run wizard | The unconditional `fmt.Println` at line 25 that makes §14.1 reachable |
| `memory/writer.go` | Debounced, atomic (temp+fsync+rename) persistence | The actual mechanism behind every "crash-safe" claim in the codebase |
| `memory/store.go` | Legacy, non-atomic JSON store, still wired for backward compat | The second, weaker persistence path described in §14.3 |
| `recall/recall.go` | Single content-addressed memory-write gateway | Documents its own reason for existing (many uncoordinated write sites, historically) |
| `llm/client.go` | Generic OpenAI-shaped HTTP client used for every provider | The real architecture behind "12 supported providers" |
| `llm/retry.go` | Retry/backoff/quota-classification layer | The most consequential correctness code in the model layer |
| `provider/embedded/governor.go` | Local-model resource-fit planner | Genuine, tested capacity-planning logic (RAM + VRAM), not a stub |
| `security/injection.go` | Prompt-injection pattern scanner | Real, specific, checkable implementation of a headline security claim |
| `security/secrets.go` | Credential pattern scanner | 13 real regexes for major credential formats (AWS, GitHub, Slack, Google, Stripe, JWT, PEM keys, etc.) |
| `safeurl/safeurl.go` | Dial-time SSRF/air-gap guard | Defeats DNS-rebinding TOCTOU specifically, security-engineering-grade |
| `tools/backend.go` | Local/Docker/SSH execution backends | Docker hardening (`--cap-drop ALL`, `--network none`, `no-new-privileges`) confirmed real in the constructed argv |
| `tools/registry.go` | Tool dispatch + checkpoint snapshot trigger | Confirms "checkpoint before every mutating action" at the exact call site |
| `tools/terminal.go` | Shell execution + sandbox refusal | `Sandbox.MustRefuse()` is genuinely called here (line 83), confirming strict-mode enforcement is real, not just a startup warning |
| `server/server.go` | HTTP route table + global middleware wrap | Confirms real routes matching every README-claimed endpoint; middleware chain includes DNS-rebinding-aware CSRF |
| `server/mcp.go` | MCP server implementation | Confirms the "both MCP client and server" claim |
| `acp/acp.go` | Agent Client Protocol server | Correct, spec-accurate NDJSON-RPC-2.0 framing |
| `app_wireup.go` (root) | Full dependency-injection graph for the whole binary | The single highest-value read for understanding how everything connects |
| `.arch-baseline` + `scripts/arch-check.sh` | Declared, CI-enforced architectural boundaries | Converts most of §8's dependency claims from inference to fact |
| `config/providers.go` | Static provider/model catalogue | Shows every provider's base URL is an OpenAI-compat surface |
| `config/policy.go` | "Can only restrict" policy engine | A real, tested, one-directional-only tightening mechanism (§7) |
| `app_postturn.go` (root) | Shared post-turn hook (plan refresh, turn_end) | Small file, but the concrete fix for "the web ran 7 post-turn steps, the console ran 1, others ran 0" |

## 14. Risks

Prioritized per the requested ordering (correctness → data corruption → concurrency → security →
...). Confidence reflects how directly each was verified in this pass; all five below were traced
to the exact triggering condition, not inferred from a pattern.

1. **[CORRECTNESS / AVAILABILITY, HIGH SEVERITY, HIGH CONFIDENCE] `darkcode --acp` on a fresh,
   unconfigured install corrupts the ACP protocol stream instead of starting the editor session.**
   `main.go:138-141`: `if err := cfg.Validate(); err != nil { if !uiMode && !guiFlag { …
   RunInteractiveSetup(cfg) … } }`. The condition checks `uiMode` and `guiFlag` but never `acpFlag`.
   `cfg.Validate()` (`config/config.go:726-739`) fails whenever `!cfg.EnableLocalLLM &&
   cfg.APIKey == ""` — the exact state of a fresh install following the README's own quick-start
   (`darkcode --acp` with no prior `--add-model`). `config.RunInteractiveSetup`
   (`config/setup.go:14-39`) calls `fmt.Println` starting at line 25, unconditionally, with no TTY
   check before it, and then blocks on `readline.Readline()` waiting for terminal input. Under
   `--acp`, stdin/stdout are the editor's JSON-RPC pipe, not a terminal — `app_acp.go:39`'s own
   comment states "nothing else may write to it" for exactly this reason. Net effect: the editor's
   `initialize` handshake never gets a valid JSON-RPC reply, and the process is simultaneously
   blocked waiting for terminal input that will never arrive over that pipe. *Recommended fix*:
   include `acpFlag` in the `main.go:139` condition (route ACP-mode validation failures to a
   stderr message, matching the `uiMode`/`guiFlag` branch, never to the interactive wizard).
   *Existing controls*: none found — this path has no test coverage (no test exercises
   `main()`'s flag-branching logic directly, which is typical for a `main` package but leaves this
   specific interaction unverified until now).

2. **[SECURITY, HIGH SEVERITY, HIGH CONFIDENCE] Secret scanner does not run under Relaxed safety
   level for non-central files.** `permission/gate.go:412-415` returns `true` (approved, no
   prompt) for `LevelRelaxed` whenever the call is not "central" (blast-radius-escalated) — before
   `argsContainSecret` is ever evaluated (first call site at `gate.go:432`; confirmed via
   `grep -rn argsContainSecret` that these are the only two call sites in the codebase, both
   strictly after the Relaxed fast-path return). A tool call under Relaxed safety whose arguments
   contain a credential-shaped string (matching any of the 13 patterns in `security/secrets.go`)
   on a non-structurally-central file is approved with no scan, no warning, no prompt. `gate.go:50`
   documents Relaxed as "auto-approve everything" by design, so a maintainer may consider this
   intentional — but it directly contradicts the README's *unqualified* "credentials in tool args
   force a prompt" claim for a level a user can select via `/safety relaxed` or config. *Recommended
   fix*: run the secret check unconditionally before (or independent of) the Relaxed fast path,
   mirroring how the blast-radius check already runs first. *Existing controls*: none found for
   this specific interaction — it is the one gap in an otherwise carefully ordered gate.

3. **[DATA INTEGRITY, MEDIUM SEVERITY, HIGH CONFIDENCE] A second, non-atomic persistence path
   coexists with the real one.** `memory/store.go`'s legacy `Store` type does a plain
   `os.WriteFile` on every mutation (no temp file, no fsync, no atomic rename), and is still
   constructed and wired at startup (`app_wireup.go:153-156`) for backward-compatible tool support,
   alongside the genuinely crash-safe `memory.System`. A crash mid-write to `memory.json`
   specifically (not the newer JSON stores) can leave a truncated/corrupt file. *Confidence*:
   directly read and confirmed; *severity* is medium because this is explicitly legacy/compat code,
   not the primary path, and no test exercises the corrupt-file-on-restart case either way
   (untested in both directions).

4. **[CORRECTNESS, LOW-MEDIUM SEVERITY, HIGH CONFIDENCE] The same legacy store matches entries to
   mutate/delete by substring, not by identity.** `memory/store.go:88-113`: `Replace`/`Remove`
   locate the target entry via `strings.Contains(entry.Content, oldText)`. Two entries whose
   content shares a common substring can have the wrong one silently mutated or deleted — this is
   an independent correctness defect from the non-atomicity in risk 3 above (that one is about
   crash safety; this one produces a wrong result with no crash at all). *Severity* is low-medium
   because it's confined to the legacy path, but it's a real, reachable defect in code that is
   still wired into the running binary, not dead code.

5. **[CONCURRENCY, MEDIUM SEVERITY, HIGH CONFIDENCE — self-acknowledged in code] Routing mode,
   safety level, and the brain/local-preference selector are all last-writer-wins across
   overlapping requests.**
   `orchestrator/request_state.go` (read in full) confirms this precisely. Four per-request
   settings (Loop, Tools-disabled, Read-only, Plan-forced) used to be single shared `*bool` fields
   on the `Kernel`, and the file's own comment documents the exact, previously-reproduced failure:
   "request A asks for `/loop`, request B asks for no loop, and A stops looping"; worse on tool
   scope, "a Chat turn pinned read-only starts answering with the mutating toolset because an
   unrelated Build turn started after it." **This has been fixed for those four settings** by
   moving them onto `context.Context` (genuinely per-request, no shared field, no lock needed —
   `request_state.go:34-141`). **It has explicitly NOT been fixed for Mode, Safety level, and the
   Brain selector** — the file's own comment states they "live on shared router/gate objects that
   a request does not own, so they still use the save/depth/restore mechanism... until then they
   remain last-writer-wins while requests overlap" (`request_state.go:28-32`). This means two
   concurrent turns, one requesting `strict` safety and another requesting `relaxed`, can currently
   have the first turn's tool calls evaluated under whichever level the second turn's in-flight
   override last wrote — a live, self-documented, unfixed concurrency gap, not a hypothetical one.
   *Existing controls*: the depth-counter save/restore correctly returns state to baseline after
   each request finishes (so it doesn't leak permanently), but does not correctly isolate two
   *simultaneously in-flight* requests, which is exactly the gap description above.

6. **[SECURITY, LOW-MEDIUM SEVERITY, MEDIUM CONFIDENCE] No HTTP authentication layer on the GUI
   server.** Confirmed no auth/session-token middleware beyond rate-limiting/CSRF/CORS
   (`server/middleware.go`). The security boundary is entirely "binds to 127.0.0.1 only" (verified
   real — `main.go` hardcodes the bind address; no `--serve` flag or code path exists to bind
   elsewhere). Anyone with local access, or any other local process/container sharing the loopback
   namespace, can reach the full API including `/v1/chat/completions` (which can drive arbitrary
   tool execution) with zero credentials. This is plausibly an intentional trust boundary for a
   local-first tool, but it is not documented as a deliberate trade-off anywhere this pass found,
   and is worth explicit confirmation with the maintainers.

7. **[OPERATIONAL, LOW SEVERITY, LOW CONFIDENCE — genuinely UNKNOWN] DAG crash-resume may not
   actually resume across a process restart.** The resume-skip-completed-tasks *code path* exists
   and is invoked (§11), but the specific scenario of a killed process restarting and correctly
   resuming was not independently exercised, and no dedicated test simulating a process kill was
   found. *Recommend*: a maintainer or follow-up pass trace `dag/`'s and `execlog.go`'s startup
   path end-to-end, or add an integration test that kills and restarts the process mid-run.

8. **[TECHNICAL DEBT / RISK-ADJACENT] Two documentation-referenced files are absent from this
   checkout.** `README.md` links `docs/POLICY.md`, `docs/THREAT_MODEL.md`, and `docs/BENCHMARK.md`;
   `scripts/arch-check.sh`'s own comment references a `THESIS.md`. None of the four exist in this
   working tree — `docs/` contains only an `images/` subdirectory (**FACT**, directly checked).
   This may simply mean this checkout is missing files present in the canonical repo, but as
   checked out, a reader following the README's own links to the threat model and policy reference
   will find nothing.

## 15. Technical Debt

- **Dual memory-persistence paths** (§14.3) — the legacy `Store` should either be migrated onto
  the same atomic-writer primitive or fully retired; currently it's neither.
- **24 `memory-writes` and 13 `llm-calls` boundary violations, tracked but not eliminated** — the
  project's own tooling frames these explicitly as a ratcheting migration in progress, not a
  finished state (`scripts/arch-check.sh`'s own header comment says exactly this).
- **`ui/` and `cli/tui/` have zero tests** — low severity given their small size (1 and 2 files
  respectively), but `ui/events.go` specifically is the shared `EventEmitter` every surface's
  live-progress reporting depends on, and it being untested is a real, if narrow, gap.
- **Provider abstraction is thinner than "12 supported providers" implies** — one generic client +
  N OpenAI-compat endpoint configs is a legitimate, lean design (and consistent with the "zero
  heavy deps" philosophy), but it means any provider-specific feature not exposed through that
  provider's OpenAI-compatibility shim is likely unreachable without new client code. Not a defect,
  but worth the team explicitly documenting as a chosen trade-off rather than leaving implicit.
- **`intelligence/treesitter.go` is misleadingly named** — despite the name, it is a `go/ast`-based
  parser for Go specifically, and is architecturally the *opposite* of tree-sitter (used instead of
  it, per the file's own header comment explaining the zero-dependency design goal). A future
  contributor searching for the tree-sitter integration will not find one; the actual multi-language
  parsing is `intelligence/langparse.go`'s regex-based pattern extraction.
- **Cognition-cascade calibration thresholds appear to reset on every process restart** (§11) — if
  confirmed, this means the "gets cheaper and smarter the more you use it" framing resets its
  per-rung learning daily/per-session for anyone who doesn't run the process continuously, which
  may be worth either fixing (persist and reload thresholds) or documenting as expected.

## 16. Unknowns

Consolidated from every subsystem pass (full detail in `docs/recon/*.md`):
- Whether DAG runs (`SetRunsDir`) actually resume execution after a real process crash, or only
  journal for inspection (§11, §14.7).
- Whether cognition-cascade calibration thresholds are reloaded from `cascade_log.jsonl` at
  startup, or reset to defaults every process restart — no load code was found, but this is not a
  certainty (§11, §15).
- Exact internals of `adjudicate/`'s consensus claim-verification algorithm body (the *decision
  tree* — evidence vs. debate vs. synthesis — was confirmed; the graph-claim-checking internals
  were not read line-by-line).
- `router/role_tracker.go`'s and `llm/keypool.go`'s exact scoring/rotation algorithm bodies (both
  confirmed real and wired via their doc comments and call sites; internals not read exhaustively).
- Depth of `debugger/dap.go`'s (818 lines) Python/JS Debug Adapter Protocol integration — file
  presence and the package's overall design confirmed; this specific file's internals not read.
- Whether the GUI frontend (`server/web/`) is hand-authored or built by a tool outside this repo —
  no `package.json` is present in this checkout, consistent with either explanation.
- Whether `RegisterResearchTool`'s SSRF-guard routing (confirmed for the dedicated `research` tool)
  extends identically to `web.go`'s standalone `web_fetch`/`web_search` tools — both were confirmed
  to depend on `safeurl` at the package level, but the exact call site inside `web.go` was not
  individually re-verified in consolidation.
- Whether the four absent documentation files (§14.8) exist in the canonical upstream repository
  and were simply excluded from this checkout, or represent files referenced but never written.
- Git history is entirely unavailable (no `.git` directory) — all "historical" claims in this
  report come from source-code comments describing prior states, not from commit history. These
  are treated as FACTs about *documented* history (the comment exists and is corroborated by the
  surrounding code), not verified against an actual commit log.

## 17. Recommended Reading Order

For a new engineer joining this codebase:

1. `README.md` — vocabulary and headline claims (read skeptically; cross-check against code, as
   this report did — several claims here were found overstated or, in one case, contradicted).
2. `main.go` — flags, startup modes, help text (doubles as an architecture summary; also read
   §14.1 alongside this file).
3. `app.go`, `app_wireup.go` (root) — the full dependency-injection graph; **the single
   highest-value read in the repository**.
4. `.arch-baseline` + `scripts/arch-check.sh` — the project's own declared architecture rules; run
   `scripts/arch-check.sh --list` yourself to see exactly which sites are grandfathered exceptions.
5. `core/` — shared vocabulary types every other package builds on.
6. `uiport/uiport.go` — the mandatory single entry point into the kernel; read its full doc
   comment, it explains a real historical bug and why the package's shape prevents a recurrence.
7. `orchestrator/kernel_execute.go` — the per-turn control flow; read alongside `cascade.go` and
   `request_state.go`.
8. `memory/system.go` + `memory/writer.go` + `recall/recall.go` — the persistence model and its
   single write gateway.
9. `permission/gate.go` + `security/` — the approval chain and content-safety scanners; read the
   exact ordering carefully (this is where §14.2's confirmed gap lives).
10. `router/router.go` + `llm/client.go` — model selection and the generic-client architecture.
11. Pick one surface end-to-end: `server/openai_compat.go` → `uiport` → kernel is the shortest,
    cleanest full trace available.
12. `config/providers.go` + `config/config.go` + `config/policy.go` — the entire feature surface,
    legible from schema, plus the restriction-only policy engine.
13. Skim `tools/registry.go` and `tools/backend.go` for how a tool call actually executes.
14. `docs/recon/*.md` — the five detailed subsystem sub-reports this report was consolidated from
    (orchestrator, memory, tools/permission/security, interfaces, model layer), for line-level
    depth beyond what's summarized above.

---

*Compiled from direct source reading, five parallel subsystem investigations, `go list`,
`scripts/arch-check.sh --list`, `go build`/`go vet`/`go test`/`go test -race` (all re-run directly
during this pass), and targeted greps. No `.md`/README content was accepted as evidence without
independent verification against `.go` source. Three claims in an earlier draft of this report
(an "empty" `concurrency/` package, a "20 of 24" memory-writes attribution, and an under-count on
`func Test` definitions) were found incorrect during consolidation and are corrected above — noted
here in the interest of the same evidentiary discipline this report asks of its own sources. No
application code was modified.*
