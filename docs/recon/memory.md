# Recon: Memory / Recall subsystem (Tier 1)

> **Note:** this is a point-in-time subsystem pass, one of five inputs consolidated into `../../DARKCODE_RECON_REPORT.md`. Several items marked UNKNOWN or unverified here (notably: the SSRF/air-gap dial-time logic in `safeurl/safeurl.go`, `tools/terminal.go`'s `Sandbox.MustRefuse()` call, prompt-cache mechanics, and the GPU `-ngl` layer-count calculation) were resolved during consolidation. **Where this file and the consolidated report disagree, the consolidated report is authoritative.**


Scope: `memory/` (59 files, ~14.4k LOC — largest subsystem by LOC in the repo), `recall/` (2 files).
Method: direct source read (not README). FACT = cites a file; INFERENCE = interpretation; UNKNOWN = unresolved.

## Purpose
Durable, system-wide (per `~/.darkcode/memory`) storage for four memory tiers (STM, episodic,
semantic, procedural/skills) plus a repository knowledge graph (KG) with structural analysis
(health, cycles, blast-radius, patterns) and a background watcher daemon. `recall/` is a thin
gateway package that is the single funnel for *writing* facts into these stores.

## Entry Points
- `memory.NewSystem(dataDir string) (*System, error)` — `memory/system.go:109`. Called once from
  `app_wireup.go:initMemoryAndProjects` at `~/.darkcode/memory` (via `defaultDarkcodeDir`).
- `recall.New(store Store) (*Manager, error)` — `recall/recall.go:159`. Called once in
  `app_wireup.go:initTools`, wraps the `*memory.System`.
- `memory.NewHealthDaemon(kg *KnowledgeGraph, dir string) *HealthDaemon` — `memory/kg_daemon.go:90`.

## Public API (representative, not exhaustive)
- `System`: `STMGet`, `Consolidate(max int) int`, `OnNewSession(func())`, `SetEmbedder`, `KG()`,
  `Shutdown()`, plus tier-specific adders (`SemanticAdd`, `EpisodicAdd`, `ProceduralAdd` — these
  three are exactly the `recall.Store` interface, `recall/recall.go:145-150`).
- `recall.Manager`: `Remember(Fact) error`, `RememberAll(...Fact) error`, `Address(Fact) string`.
- `recall.Graph(m *Manager) core.KnowledgeGraphStore` — adapter so graph-sync code that already
  speaks `AddNode`/`AddEdge` routes through the gateway (`recall/recall.go:266-306`).

## Internal Components
- `store.go` — **legacy** `Store` type: flat JSON array (`memory.json`), full-file
  marshal/rewrite on every `Add`/`Replace`/`Remove` (`memory/store.go:56-66`, no debounce, no
  atomic rename — plain `os.WriteFile`). This is the `oldStore` built in
  `app_wireup.go:initTools` (`memory.NewStore(filepath.Join(memDir,"memory.json"))`) — kept
  alive only for `tools.RegisterBuiltinTools`/`NewSemanticMemoryTool` backward compat. **This is
  a second, non-atomic persistence path living alongside the real one** — see Risks below.
- `writer.go` — `DebouncedWriter`: coalesces writes (default interval 2s, `NewDebouncedWriter`,
  `memory/writer.go:55`), and `atomicWriteFile` (`memory/writer.go:139-163`): temp file in same
  dir → `Write` → `Sync` (fsync) → `Close` → `Chmod 0600` → `os.Rename`. This is the *actual*
  crash-safe path used by episodic/semantic/procedural/architecture writers
  (`memory/system.go:142-`). **FACT: the "atomic, crash-safe" README claim is true for the
  current System, verified against real temp+fsync+rename code.**
- `system.go` (922 lines) — the `System` struct: STM (`stm []core.Message`, cap 50,
  `memory/system.go:116`), a separate uncapped-but-bounded `transcript` for overflow, episodic/
  semantic/procedural/architecture stores each JSON file + `DebouncedWriter`, an `embedCache`
  (exact-text-keyed, deliberately no near-miss fuzzing — comment at `system.go:38-39`), and
  `sessionEpoch` for session-boundary filtering.
- `knowledge_graph.go` (644 lines) + `kg_*.go` (health, query, answer, simulate, policy, history,
  patterns, forecast, context, adjudicate) — the KG and its ~10 analysis modules.
- `kg_daemon.go` — background watcher (`HealthDaemon`), self-throttled to a CPU-time budget.
- `embed_check.go` — `ValidateEmbedder`, a **real** semantic-quality gate (see below).
- `skillimport.go` — procedural-memory (skills) importer, called once at startup
  (`app_wireup.go:733`, `a.importSkills()`).
- `decay.go`, `learning.go`, `defects.go`, `replay.go`, `compose.go`, `retrieval.go` (809 lines) —
  confidence decay, outcome-driven learning engine, defect-risk scoring, episodic replay for
  `/log`, prompt-context composition, and the hybrid (keyword+vector) retriever.

## Dependencies
`memory` imports: `core`, `intelligence` (parsing), `internal/strutil`, `observability`. (per
`go list` dep graph gathered by parent.) `recall` imports only `core`.

## Dependents (fan-in)
`memory` has fan-in 10 (per parent's `go list` graph) — `tools`, `orchestrator`, `server`, `cli`,
`ingest`, `candidate`, `selfheal`, `eval`, `datasource`, root `main`. `recall` fan-in: `orchestrator`
(via `SetRecall`), `tools`, `ingest`, `eval`, root `main`.

## Data
- On disk, per data dir (`~/.darkcode/memory/` by default, or `MemoryDir` config override):
  `memory.json` (legacy Store, non-atomic), `episodic.json`, `semantic.json`, `procedural.json`,
  `architecture.json`, `cascade_log.jsonl` (path set by orchestrator, not memory itself),
  `health_history.json` (HealthDaemon series), `model_reliability.json` (router, not memory).
- **FACT: no database.** Every store found is a JSON file read whole into memory at load and
  rewritten whole (debounced) on mutation. No SQLite/embedded-DB import anywhere in `memory/`'s
  or `recall/`'s dependency list. README's "no database" claim verified.
- KG nodes/edges: `core.KGNode`, `core.KGEdge` (defined in `core/`, not `memory/` — `core` is the
  shared vocabulary package, fan-in 27, the highest in the whole repo per parent's graph).

## Control Flow (representative: a tool call that touches memory)
1. Tool executes, calls e.g. `recall.Manager.Remember(recall.Entity{Node: n})`
   (`recall/recall.go:172`).
2. `Remember` type-switches on the `Fact` shape (`Entity`/`Link`/`Note`/`Event`/`Procedure`) and
   calls the matching `Store` method (`KG().AddNode`, `SemanticAdd`, `EpisodicAdd`, ...).
3. The `System` method acquires `s.mu`, mutates the in-memory slice/map, calls
   `xWriter.MarkDirty()` (`memory/writer.go:68`).
4. `MarkDirty` sets an atomic dirty flag and (if no timer already pending) schedules
   `w.flush()` via `time.AfterFunc(2s, ...)`.
5. `flush()` re-serializes the *entire* current in-memory collection (`serialize()` closure
   re-reads `s.mu.RLock()`'d state) and calls `atomicWriteFile` (temp+fsync+rename).
6. On graceful shutdown (`app.go:gracefulShutdown` → `a.MemSystem.Shutdown()`), each writer's
   `Shutdown()` does one final synchronous flush if dirty.

## External Effects
- Writes JSON files under the memory data dir.
- Optionally calls an embedder LLM client (`CreateEmbedding`) — network call if the embedder is
  a cloud model, local call if `provider/embedded`.
- `HealthDaemon` reads the KG in-process only; no external effect besides its own history file.

## Business Rules
- **Content-addressed dedup**: every `Fact`'s `address()` is a hash of its *meaning*, excluding
  provenance/confidence/timestamp, specifically so re-observing the same fact (e.g. re-reading an
  unchanged file) overwrites rather than duplicates (`recall/recall.go:26-30, 92-141`).
- **Embedder must pass a quality gate before being wired in**: `ValidateEmbedder`
  (`memory/embed_check.go:35`) embeds 6 labeled sentence pairs (3 similar / 3 dissimilar,
  deliberately low keyword overlap so keyword-matching can't fake a pass) and requires the mean
  cosine-similarity gap between the two groups to exceed `embedValidationMargin = 0.10`
  (`embed_check.go:31`). A degenerate/near-constant embedder (common failure mode for small local
  models) is *rejected*, falling back to keyword-only recall — wired at
  `app_wireup.go:441-467` (`wireEmbedder` → `memory.ValidateEmbedder` → `a.MemSystem.SetEmbedder`).
  **FACT, and it's a real check, not a stub** — reads actual vectors, computes real cosine sim.
- **Session boundary**: `sessionEpoch` (`system.go:61`) — episodic recall ignores conversation
  entries older than the current epoch; `/new` / `/api/reset` bump it via `StartNewSession`.
  `OnNewSession` callbacks (list, not single) fire on that boundary; `app_wireup.go:723-727`
  registers one that calls `Consolidate(EpisodicMaxEntries)` and logs how many entries were
  forgotten.
- **KG parsing is language-tiered, not uniform under the hood** (partially contradicts a literal
  reading of "one uniform shape" if taken to mean one parser): `intelligence/treesitter.go` is,
  despite its filename, a `go/ast`-based parser for Go only (comment at
  `intelligence/treesitter.go:3-9` explicitly: "spec names tree-sitter as the parsing backbone...
  for Go projects the stdlib go/ast gives exact structural information with no external
  dependency, so this is Go-first"); `intelligence/langparse.go` (`LanguageOf`, `ParseText`)
  handles the other languages via text/regex pattern extraction, not a real tree-sitter grammar.
  **No tree-sitter dependency exists anywhere in `go.mod`** — confirms "dependency-free," but the
  filename `treesitter.go` is misleading (it's the *opposite* of tree-sitter: a Go-specific AST
  parser used *instead of* tree-sitter).

## State
`System` holds all four memory tiers plus the KG as in-process state guarded by one `sync.RWMutex`
(`system.go:25`). `HealthDaemon` holds its own `sync.Mutex`-guarded history/alerts.

## Concurrency
- `System.mu sync.RWMutex` guards all tier mutations; writers run on a `time.AfterFunc` goroutine
  independent of the caller.
- `DebouncedWriter` has its own `sync.Mutex` (guards the timer) plus `atomic.Int32`/`Int64` for
  the dirty flag and write/error counters — a documented historical bug is recorded in the code:
  the atomic counters MUST be the first struct fields on 32-bit platforms or `atomic.AddInt64`
  panics with "unaligned 64-bit atomic operation" (`memory/writer.go:36-41`, "This was a real
  crash on windows/386"). This is a genuine, previously-shipped concurrency bug now fixed and
  guarded by a comment (not by a test I could find — see Tests).
- `embedWG sync.WaitGroup` (`system.go:105`) tracks in-flight async embedding backfills so
  `Shutdown()` can wait for them — a real, deliberate mechanism to avoid losing a vector that was
  mid-flight at process exit.
- There is a dedicated test file `memory/system_concurrency_test.go` — see Tests.

## Error Handling
- Store load failures (`loadEpisodic` etc.) are fatal at `NewSystem` construction — the whole
  process exits (`app_wireup.go:128-132` `os.Exit(1)` on `memory.NewSystem` error).
- Debounced-writer flush failures re-mark dirty and retry on the next interval rather than
  dropping the write (`writer.go:115-129`).
- `recall.Manager.Remember` returns per-fact errors; `RememberAll` continues past individual
  failures and returns only the first error (`recall/recall.go:225-233`) — a bulk KG sync of
  hundreds of nodes isn't aborted by one bad node.

## Tests
- Present: `system_concurrency_test.go`, `knowledge_graph_test.go`, `kg_health_test.go`,
  `kg_daemon_test.go` (324 lines — substantial), `kg_answer_test.go`, `kg_simulate_test.go` (via
  file listing — not all opened), `embed_check_test.go`, `cosine_precision_test.go`,
  `retrieval_test.go`, `retrieval_vector_test.go`, `retrieval_determinism_test.go`,
  `retrieval_bench_test.go`, `defects_test.go`, `decay_test.go`, `replay_test.go`,
  `skillimport_test.go` (500 lines, larger than the implementation file it tests — thorough),
  `fileobs_test.go`, `writer_align_test.go` (**almost certainly the 32-bit-alignment regression
  test for the writer.go bug above** — name strongly implies it, not opened to confirm exact
  assertions), `recall/recall_test.go`.
- No `store.go` (legacy Store) test file found in the listing — the non-atomic legacy JSON path
  appears untested.
- UNKNOWN: whether any test actually exercises a simulated crash mid-write (kill process during
  `atomicWriteFile`) — the atomic-rename *design* is sound, but I did not find a test that
  verifies recovery from a real interrupted write (e.g., a leftover `.tmp-*` file check).

## Important Files
| File | Purpose |
|---|---|
| `memory/system.go` | Central `System` type: STM/episodic/semantic/procedural + wiring |
| `memory/writer.go` | Debounced, atomic (temp+fsync+rename) persistence primitive |
| `memory/store.go` | Legacy non-atomic JSON store, still wired for backward compat |
| `memory/knowledge_graph.go` | Core KG type: nodes, edges, stats |
| `memory/kg_daemon.go` | Background health-watcher, CPU-budgeted |
| `memory/kg_health.go` | Repository health scoring |
| `memory/kg_answer.go` | Answers `graph_query` tool questions (blast_radius etc., largest kg_* file) |
| `memory/kg_simulate.go` | "what if" structural simulation |
| `memory/kg_patterns.go` | Convention-mining (`MinePatterns`) feeding `PatternLibrary` |
| `memory/kg_policy.go` | Declared-architecture policy checking |
| `memory/kg_history.go` | Structural git-independent evolution tracking |
| `memory/embed_check.go` | Real semantic-quality gate for embedder clients |
| `memory/retrieval.go` | Hybrid (keyword + vector) recall (809 lines) |
| `memory/skillimport.go` | Procedural-memory (skills) import at startup |
| `memory/decay.go` | Confidence decay over time |
| `memory/defects.go` | Defect-risk / root-cause scoring |
| `recall/recall.go` | Single write gateway, content-addressed dedup |
| `intelligence/treesitter.go` | Go-only `go/ast` parser (misleadingly named) |
| `intelligence/langparse.go` | Non-Go language pattern/regex extraction |

## Unknowns
- Exact algorithm detail inside `kg_answer.go` (blast_radius, cycles, dead_code) — file exists and
  is the largest `kg_*` file (472 lines) implying real logic, but I did not read the algorithm body
  line-by-line to confirm sophistication vs. heuristic simplicity.
- Whether `memory-writes 24` (`.arch-baseline`) violation sites are mostly the legacy `store.go`
  path, KG-sync bulk writes, or genuinely scattered bypasses — did not enumerate them (would need
  `scripts/arch-check.sh --list`).
- Whether `System.mu` (one coarse RWMutex over 4 tiers + KG) is a contention bottleneck under
  concurrent sub-agent tool calls — plausible given the single-mutex design but unmeasured.
- Whether `oldStore`/legacy `store.go` writes are ever reconciled with the `recall` gateway's
  dedup, or represent a second source of truth that can drift from it — code shows they are
  separate files/structs entirely, so drift seems architecturally possible; not confirmed with a
  concrete reproduction.

## Documented-claim verification table

| Claim (marketing vocabulary) | Verdict | Evidence |
|---|---|---|
| "Persistent... system-wide, shared across all your projects" | VERIFIED | `defaultDarkcodeDir` roots at `~/.darkcode/memory`, one dir for all invocations (`app_wireup.go:99-108`) |
| "No database" | VERIFIED | Every store is a flat JSON file; no DB import in dependency graph |
| "Zero heavy deps" (for memory/KG specifically) | VERIFIED | No tree-sitter, no DB, no vector-index library; `go.mod` has 4 non-indirect deps, all TUI |
| "Writes are atomic (crash-safe)" | VERIFIED (for current System) | `atomicWriteFile`: temp file, `Sync()`, `Rename()` (`writer.go:139-163`) |
| ...but legacy `memory.json` path is non-atomic | CONTRADICTS the blanket claim | `store.go:56-66` plain `os.WriteFile`, no temp/rename/fsync |
| "Optional local embedder turns recall genuinely semantic" | VERIFIED, and better than claimed | Real quality-gated validation (`embed_check.go`), not just "optional" — actively rejected if it fails a cosine-margin test |
| "Go, TypeScript/JS, Python, Rust, Java... one uniform shape... difference recorded as node confidence" | PARTIALLY VERIFIED | Go via real AST (`treesitter.go`, despite name); others via `langparse.go` regex/pattern extraction, consistent with README's own "dependency-free pattern scanner" framing — did not verify the "confidence" field is actually set differently per language (UNKNOWN) |
| Health daemon "holds to 5% of one core by default... measuring each scan and resting proportionally" | VERIFIED | `DefaultCPUPercent = 5` (`kg_daemon.go:33`), `SetCPUPercent` clamps 1-50 (`kg_daemon.go:99-112`), loop design described as scan-time-proportional sleep in the file's own header comment |
| "Alerts fire on transitions... a cycle that has been there for a year stays quiet" | VERIFIED (by design/comment) | `kg_daemon.go:44-48` `Alert` doc explicitly states transition-only firing; did not trace the diffing logic itself line-by-line |
