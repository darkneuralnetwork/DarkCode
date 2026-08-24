# DarkCode — Codebase Size Analysis & Reduction Options

**Scope:** read-only research, no code modified. Commissioned to answer: "competitors are
5k-10k LOC, why is this ~96k LOC, and how do we cut it significantly." Method: direct
measurement of this repo (same methodology as the architecture recon: `wc -l`, `go list`,
targeted greps for dead-code candidates) plus real, freshly-cloned comparison repos —
not recalled/estimated figures. FACT/INFERENCE discipline carried over from the prior audit.

---

## 1. The headline number, precisely

- **95,894 total Go LOC** across 504 files: **67,932 non-test** (299 files) + **27,962 test**
  (205 files) — a ~41% test-to-source ratio (**FACT**, direct `wc -l`, matches the earlier
  architecture recon exactly).
- Zero external runtime dependencies beyond four TUI libraries (`bubbletea`, `lipgloss`,
  `readline`, `x/sys` transitively) — **FACT**, `go.mod`. No database driver, no HTTP framework,
  no JSON-RPC library, no LSP/DAP client library, no tree-sitter binding, no retry/rate-limit
  library, no multi-provider LLM SDK.

## 2. Is "competitors are 5-10k LOC" actually true? — Tested against three real repos

I shallow-cloned three widely-used agent projects and counted source LOC with the same exclusions
(no vendor/node_modules/dist, tests counted separately) used for DarkCode above.

| Project | Language | Non-test source LOC | Test LOC | What it does *not* build itself |
|---|---|---|---|---|
| **opencode** (`sst/opencode`, ~143k GitHub stars) | TypeScript | **~9,936** | not separated in this pass | Persistent state via **real SQLite**, not a hand-rolled store; multi-provider access via the Node/npm ecosystem (provider SDKs pulled from `node_modules`, invisible to this LOC count); TUI-only — no web GUI, no ACP, no local-model hosting/resource governor, no knowledge graph, no LSP/DAP integration, no sandboxing backends, no checkpoint/rollback content store, no plugin host, no policy engine, no benchmark harness |
| **aider** (`Aider-AI/aider`) | Python | **~25,853** | **~12,428** (≈38.3k total) | Multi-provider access via `litellm` (a library); real multi-language parsing via actual **tree-sitter** bindings (`.scm` query files present in the repo — a dependency, not a reimplementation); undo/history via **git itself**, no custom checkpoint store; no GUI, no local-model lifecycle management, no MCP/ACP server, no plugin system, no policy engine |
| **goose** (`block/goose`) | Rust | **~227,136** | not separated in this pass | A full platform of comparable ambition to DarkCode (MCP extensions, recipes, desktop app) — and **more than 3x DarkCode's total size**, despite Rust's terser syntax than Go for equivalent logic |

**Conclusion, stated plainly:** the "5-10k LOC" figure is real for **opencode specifically**, but
opencode is not a fair apples-to-apples comparison — it is a thinner product (TUI-only, one
protocol surface, a real database, provider complexity outsourced to npm packages) doing a
narrower job. The moment a competitor's *ambition* matches DarkCode's ("platform," not "loop") —
goose is the clearest example — the competitor is **larger**, not smaller. Aider, which is
tightly, deliberately scoped to one job (make an LLM edit files well) and *still* leans on two
real dependencies (`litellm`, tree-sitter) to stay lean, is already **~38k lines total** —
well above "5-10k" — despite doing a fraction of what DarkCode does.

**INFERENCE:** the "5-10k" benchmark the request is measuring against is very likely describing
either (a) a project's core loop only, excluding its dependency tree's hidden complexity (an
npm-based tool's `node_modules` can easily contain hundreds of thousands of lines doing what
DarkCode hand-rolls), or (b) a genuinely narrower-scope tool than DarkCode currently is. Getting
DarkCode itself to 5-10k lines is not a refactoring exercise — it would require **abandoning most
of the feature list in its own README**, not just writing tighter code. The rest of this document
treats "reduce significantly" as "get meaningfully smaller while keeping most of the current
identity," and separately flags what it would take to hit the literal 5-10k figure.

## 3. Where the 67,932 non-test lines actually live

Directly measured, largest subsystems first (non-test LOC):

| Subsystem | LOC | What it is |
|---|---|---|
| Knowledge graph (`memory/kg_*.go` + `knowledge_graph.go`) | **3,995** | ~15 structural-analysis verbs: health, cycles, dead_code, blast_radius, defect_risk, root_cause, structure, simulate, patterns, policy, trends, alerts, low_confidence, stale |
| Local-model hosting (`provider/embedded/` + `capability/`) | **2,596** | llama-server subprocess lifecycle, RAM/VRAM resource governor, GPU layer-count estimation, hardware capability detection |
| `llm/` | 2,648 | Generic OpenAI-shaped HTTP client, retry/backoff, adaptive rate limiting, key-pool credential rotation, prompt-cache request shaping |
| `router/` | 2,658 | Tier resolution, escalation ladder, consensus/distribution, reliability tracking |
| Multi-language code intelligence (`intelligence/`) | 1,806 | Hand-rolled LSP JSON-RPC client (5 languages) + `go/ast`-based Go parser + regex-based parser for 4 other languages |
| Debugger integration (`debugger/`) | 1,352 | Hand-rolled Delve JSON-RPC client + DAP client (Python/JS) |
| Protocol/API surfaces (`server/mcp.go`, `htp.go`, `openai_compat.go` + `acp/`) | 1,830 | **Four** separate tool-invocation/chat protocols: OpenAI-compatible REST, MCP server, an undocumented bespoke "HTP" protocol, and ACP |
| `config/providers.go` | 487 | Static data: 19 providers × model catalogs × pricing tables, expressed as Go struct literals |

Four **independently fully-wired user surfaces** (`server/` 7.5k, `cli/`+`cli/tui/` 5.8k, `acp/`
~0.7k, `ui/` event bus 0.4k ≈ **~14.4k LOC** just for "the door in," before any of the logic behind
it) is the single biggest structural driver of size that a narrower competitor (opencode: TUI
only) simply doesn't pay.

## 4. Why it's this large — five real, distinct causes

### 4.1 "Zero heavy deps" — the project reimplements what most competitors import
This is the single largest multiplier. Every item below is a deliberate design choice (stated
explicitly in `go.mod`'s own toolchain-pin comment and the README's "zero heavy deps" principle),
not an accident — but each one trades a library import (tens of lines of glue code) for hundreds
to low-thousands of lines of DarkCode-maintained implementation:

| Capability | DarkCode's hand-rolled version | What a typical competitor imports instead |
|---|---|---|
| Multi-language code parsing | `intelligence/langparse.go` — per-language regex tables | tree-sitter binding (aider does this) |
| LSP client | `intelligence/lsp.go` — hand-rolled JSON-RPC/stdio | an existing Go LSP client library |
| Debugger bridge | `debugger/delve.go`, `dap.go` | typically omitted entirely by competitors |
| Multi-provider LLM client | `llm/client.go` + retry/ratelimit/keypool | `litellm` (Python), a provider SDK, or the Vercel AI SDK (TS) |
| Persistence | `memory/writer.go` — debounced atomic JSON writer | SQLite (opencode does this) |
| Undo/rollback | `checkpoint/` — content-addressed blob store | git itself (aider does this) |
| Sandboxing | `security/sandbox.go` — bwrap/firejail wrapping, 3 exec backends | a container SDK, or omitted |
| Plugin system | `plugin/` — hand-rolled subprocess protocol | an existing plugin framework (e.g. `go-plugin`) |
| Local-model hosting | `provider/embedded/` — full subprocess lifecycle + resource governor | delegate to Ollama's own management, or omit local models |

### 4.2 Feature breadth — many separately-substantial subsystems most competitors don't have at all
Knowledge graph, local-model hosting, LSP, debugger, checkpoint/rollback, policy engine, plugin
host + hooks, self-heal + candidate ranking, benchmark harness, health daemon, multi-backend
sandboxed execution, vault-backed credentials, secret + prompt-injection scanners — **each one is
individually comparable in scope to a whole small tool**; DarkCode bundles roughly fifteen of them
into one binary. This is a product-scope fact, not a code-quality problem.

### 4.3 Four fully-wired surfaces instead of one
CLI+TUI, web GUI (own frontend), ACP, OpenAI-compatible API — each with its own request handling
on top of the shared `uiport`/kernel. Opencode, for comparison, ships one surface (TUI).

### 4.4 Test investment
28k of 96k total lines (29%) are tests. This is not waste — the earlier architecture audit found
this test suite substantive and well-targeted (154 test functions in the tools/permission/security
cluster alone, dedicated concurrency and retrieval-determinism tests, etc.) — but it does inflate
any raw LOC comparison against a project that reports source-only numbers. Aider carries a similar
ratio (12.4k tests on 25.8k source, ~48%).

### 4.5 Verbose "why this exists" documentation comments
The house style is long, narrative doc comments explaining historical bugs and design rationale
(the entire prior architecture audit relied on these as primary evidence). Real, measurable lines;
zero logic. High value, but the longest examples run 20-30 lines for a single function and could
likely be halved without losing the substance.

## 5. Confirmed dead / redundant code — the free wins

Found by directly grepping for callers of every top-level package during this pass (not
exhaustive — a proper `staticcheck -unused` / `deadcode` sweep would likely find more):

| Finding | Evidence | LOC recoverable |
|---|---|---|
| **`eval/` package is entirely unwired.** No caller anywhere in the tree outside its own test file — not referenced by `app_wireup.go`, no CLI command, no `cmd/` wrapper (unlike `bench/`, which has `bench/cmd/benchrun`). | `grep -rn "darkcode/eval"` returns zero hits outside `eval/` itself | **523** |
| **`scheduler/` is constructed but never actually instantiated in the shipped binary.** Its only two real call sites, `app_wireup.go:341` and `:536`, both pass **`nil`** for the `*scheduler.Scheduler` parameter `provider/embedded.NewProvider` accepts. The type exists, compiles, and is fully tested — but is never a live object in `darkcode`, `darkcode --gui`, or `darkcode --acp`. | `grep -n "NewProvider(\|NewProviderWithDirs("` — both real call sites use `nil` | **656** |
| **Legacy `memory/store.go` persistence path.** Non-atomic (`os.WriteFile`, no fsync/rename) and matches entries to mutate/delete by substring rather than ID — confirmed as a live correctness+data-integrity risk in the prior architecture audit (`DARKCODE_RECON_REPORT.md` §14.3-14.4). Still constructed and wired at startup for two callers' backward compatibility. | `app_wireup.go:153-156, 172-175` | ~172 (file) + migration cost for 2 call sites |
| **`server/htp.go` — a fourth, undocumented protocol.** 431 lines implementing tool discovery/execution/batching/chaining as a bespoke JSON envelope, alongside the already-present MCP server (`server/mcp.go`) which exists specifically to do tool discovery/execution in a standard way. Not mentioned anywhere in `README.md`. **Not yet confirmed dead** (it is wired: `mux.HandleFunc("/api/htp", ...)`) — this is a *design-review* candidate, not a confirmed-dead one; the actual overlap with what MCP's current spec already covers needs a maintainer decision, not a unilateral deletion. | `server/server.go:279-280` | up to 431 + related tool-side wiring, **if** confirmed redundant |

**~1,600+ LOC of confirmed dead or clearly-legacy code found in under an hour of targeted
checking** — this is very likely a lower bound, not the full extent.

## 6. Recommendations, ranked by effort/risk vs. payoff

**Tier A — zero-tradeoff, do first (~1,200-2,500 LOC, no feature loss):**
1. Delete `eval/` (or, if it was meant to ship, wire it into `app_wireup.go`/CLI and add a `make`
   target — right now it's neither used nor discoverable).
2. Delete `scheduler/`, or actually pass a real instance into `provider/embedded.NewProvider`
   if resource-aware model loading was the intent — as written it's inert weight either way.
3. Retire `memory/store.go`; migrate `tools.RegisterBuiltinTools`/`NewSemanticMemoryTool`'s
   backward-compat callers onto `memory.System` directly.
4. Run `golang.org/x/tools/cmd/deadcode` and `staticcheck -unused` across the whole tree — this
   pass checked four suspects by hand; a real static sweep will likely surface more.
5. Externalize `config/providers.go`'s ~487 lines of pure catalog data into a `go:embed`ded
   JSON/YAML asset — identical behavior, less Go source to read/maintain.

**Tier B — real but moderate design decisions (~1,000-2,000 LOC):**
6. Decide whether `server/htp.go` needs to exist separately from the MCP server; if its
   batch/chaining/streaming claims are already covered by MCP's current spec, retire it and its
   client-side wiring.
7. Evaluate whether `candidate/` and `selfheal/` — both "apply a patch, run the verifier, keep or
   revert" logic — are earning their separation as two packages.
8. Audit real usage of the knowledge graph's ~15 analysis verbs (CLI/GUI telemetry, if any) and
   retire the ones nobody calls (candidates to check first: `forecast`, `trends`, `simulate` —
   these read as the least load-bearing of the set based on the README's own framing).

**Tier C — large cuts, genuine product/scope tradeoffs (potentially 8,000-15,000+ LOC, needs
explicit maintainer buy-in — these directly contradict stated design goals and should not be
done unilaterally):**
9. **Drop hand-rolled local-model hosting** (`provider/embedded/` + `capability/`, 2,596 LOC) and
   delegate entirely to Ollama/LM Studio's own process management, treating them as just another
   provider entry (as they partially already are) instead of reimplementing a resource governor
   and GPU layer estimator. Directly reduces the "local-first" story's independence from an
   external daemon — a real product trade, not a free win.
10. **Drop the hand-rolled LSP client** (part of `intelligence/`'s 1,806 LOC) in favor of an
    existing Go LSP client library, or drop LSP integration entirely and rely on the knowledge
    graph plus ad hoc `go vet`/`tsc`/`mypy` subprocess calls for type-checking signals.
11. **Drop `debugger/`** (1,352 LOC) if actual usage is low — this is the kind of feature that's
    impressive in a README and rarely invoked in practice; worth checking real usage telemetry
    before cutting, but it's a strong LOC-per-value outlier if usage is low.
12. **Pick one of {ACP, MCP-server, HTP}** rather than maintaining three overlapping
    tool-invocation protocols, and consider whether the web GUI needs its own custom frontend
    (`server/web/`) versus pointing an existing OpenAI-compatible chat UI (Open WebUI, LibreChat)
    at the already-implemented `/v1` endpoint — this could remove a meaningful share of `server/`'s
    7.5k LOC beyond just the API-handling portion.
13. **Accept a tree-sitter (CGO) dependency** to replace the 4-language regex-based parser in
    `intelligence/langparse.go` — likely *more* correct as well as smaller, but this is a direct
    reversal of the explicit "zero heavy deps, single static binary, `CGO_ENABLED=0`" principle
    stated in the README and enforced by the release process (`build.sh`) — a philosophy change,
    not a free refactor.

**Tier D — style/hygiene (a few thousand lines, lower priority):**
14. Shorten the longest doc-comment blocks (some run 20-30 lines per function). High value as
    documentation — this project's own comments were the primary evidence base for the entire
    prior architecture audit — so this should be done selectively, not as a blanket policy.

## 7. What a realistic target looks like

- **Tier A + B alone**: roughly **67,000 → 63,000-64,000** non-test LOC. Low risk, no feature loss,
  should be done regardless of any broader size initiative — this is just removing confirmed dead
  weight and legacy debt already flagged as a risk in the prior audit.
- **Tier A + B + C (all of it)**: plausibly **67,000 → 40,000-50,000** non-test LOC — a genuine,
  substantial reduction (~30-40%) — landing DarkCode roughly in the territory of a lean version of
  a platform-class competitor. This is **still not 5-10k lines**, because that figure belongs to a
  different *category* of product (a single-surface loop leaning on a managed-language ecosystem
  and a real database), not a smaller version of this one.
- **Reaching literal 5-10k lines** would require deleting essentially everything in Tier C *plus*
  the knowledge graph, checkpoint/rollback, policy engine, plugin/hooks system, sandboxing, and
  three of the four user surfaces — at which point the result is a different, much narrower
  product than what DarkCode's own README describes itself as being. That may be a legitimate
  strategic direction (ship a thin core now, offer the rest as optional plugins later), but it's a
  product decision for the maintainers, not a code-cleanup task.

## 8. Suggested next step (not performed — read-only per instructions)

If the goal is genuinely "reduce significantly" rather than "hit exactly 5-10k," the highest-value
sequence is: **Tier A first** (this week, no debate needed) → **run a real dead-code tool** across
the tree to find what this manual pass missed → **then** bring Tiers B/C to the maintainers as
explicit, individually-votable scope decisions, since each one trades a specific, real capability
for a specific, real LOC reduction.

---
*No application code was modified. Comparison repositories (`sst/opencode`, `Aider-AI/aider`,
`block/goose`) were shallow-cloned to a session-scratch directory for measurement only and have
been removed; nothing was committed to or copied into this repository from them.*
