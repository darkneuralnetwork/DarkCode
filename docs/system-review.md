# System review

> **Note:** this document predates the `kernel/infra/surfaces/model` directory
> restructure (August 2026). File paths it cites reflect the flat layout at
> the time of writing; the content and findings are otherwise unchanged.


A full-stack audit — backend, API, GUI, CLI — written from four seats at once:
architect, engineering manager, business analyst, and critic. Everything below
is measured against the tree, not inferred. Where a suspicion did not survive
checking, that is recorded too, because a review that only reports confirmed
findings hides how reliable its method was.

## Scorecard

| Area | State | The thing that matters |
|---|---|---|
| Backend architecture | **Good** | Clean dependency direction, no cycles |
| API surface | **Good** | No 404s, no orphan calls, method discipline holds |
| Security | **Good with one gap** | Origin-only CSRF is sound here; sub-agent scoping was the real hole and is now closed |
| Performance | **Good** | The dominant cost is 2,047 tokens of tool schema per turn |
| GUI ↔ API ↔ CLI sync | **Weak** | Three surfaces disagree about what the product is |
| Test coverage | **Uneven and risky** | `agents` at 4.4% holds the most dangerous code |
| Redundancy | **Mostly resolved** | Strategy settings retired; the remainder is documented below |

---

## 1. Backend

**Dependency direction is clean.** `orchestrator` does not import `server` — the
arrow points one way, so the kernel stays testable without an HTTP stack. This
is the single most important structural property in the codebase and it holds.

**Coupling is concentrated where you would want it.** `router` imports 4
internal packages, `memory` 5, `agents` 6, `loop` 9. The composition roots —
`server` 22, `cli` 20, `orchestrator` 19 — are high, but that is what a
composition root is for.

**Eight files exceed 780 lines**, led by `server/server.go` at 1,108 and
`permission/gate.go` at 929. None is incoherent, but `server.go` is now doing
routing, middleware, workspace state, GUI lifecycle and project summarisation.
It is the first file that will resist change.

### The real backend risk is test coverage, not structure

| Package | Coverage | What lives there |
|---|---|---|
| `agents` | **4.4%** | the sub-agent execution loop, retry ladder, tool budgets |
| `cli` | 1.7% | the whole terminal surface |
| `plugin` | 0.0% | external plugin loading |
| `tools` | 15.6% | every tool the agent can call |
| `server` | 21.0% | the entire HTTP layer |
| `compression` | 22.9% | context fitting — *where the reported 400 came from* |

`agents` at 4.4% is the finding. It contains the per-tool call budget, the
exact-repeat guard, the forced-final-answer path and the error-manager retry
ladder — control flow with many branches and almost no tests. The reported
Gemini 400 surfaced through `compression` at 22.9%, which is the same story:
**the two packages where the hard bugs live are the two least tested.**

That is a management finding more than an engineering one. Coverage in
`memory` (77.7%), `plan` (81.1%) and `checkpoint` (80.8%) shows the discipline
exists; it has just not been applied where the risk concentrates.

---

## 2. API

Cross-referenced every registered route against every call site.

**No mismatches in either direction.** 58 routes registered, 46 called by the
browser, zero calls to a route that does not exist, zero routes the GUI expects
and cannot reach. For a hand-wired mux with no code generation that is better
than it has any right to be.

Twelve routes have no browser consumer, and all twelve are deliberate:
`/api/htp` and `/api/mcp` are external protocols, `/api/health` is for
monitoring, `/api/tools/execute` and `/v1/chat/completions` are for external
clients, `/api/audit/export` is a download. The four `/api/memory/{episodic,
semantic,procedural,short-term}` routes are the only ones that look genuinely
unused — worth confirming before removal, since a CLI or external tool may
depend on them.

**Method discipline holds.** Every mutating endpoint checked (`reset`,
`rollback`, `approvals/decide`, `tools/execute`, `metrics/reset`, `fs/mkdir`)
enforces POST, verified live: `GET /api/reset` returns 405.

---

## 3. Security

### What was tested, live

| Probe | Result |
|---|---|
| `POST /api/reset` with `Origin: http://evil.com` | **403** |
| Same with `Origin: http://localhost.evil.com` | **403** — no prefix bypass |
| Same with `Origin: http://localhost:12399` | 200 |
| `GET /api/reset` (the `<img src>` vector) | **405** |
| `GET /api/definitely-not-a-route` | **404 JSON**, not the app shell |

`isLocalhostOrigin` requires a trailing colon on every prefix, so
`localhost.evil.com` cannot match `http://localhost:`. The bypass I went looking
for is not there.

### The CSRF design is defensible, and worth stating explicitly

Protection is Origin-only, with no token. A request arriving with **no** Origin
header passes. That sounds alarming and is not, for a specific reason: browsers
send `Origin` on all cross-origin `fetch`, `XHR` and form POSTs, and every
mutating endpoint refuses GET. A non-browser client omitting Origin is not a
confused deputy — it is the user's own tooling.

The assumption to keep in view: **this holds only while the server stays bound
to loopback.** The moment anything proxies it, Origin-only becomes insufficient.
The code says the server is always `127.0.0.1`; that comment is now load-bearing
security, and any future "expose on LAN" feature must add tokens first.

### The actual hole was sub-agent authority, and it is closed

Every sub-agent used to receive the entire tool registry. A `research` agent —
whose job is to fetch a page and summarise it — held shell execute and file
write. That is a two-hop prompt-injection path: untrusted page content reaches a
model that can run commands.

Now scoped by role, enforced at two layers: restricted roles are not offered
write schemas, and dispatch refuses them even if the model invents a tool name.
`SubAgentConfig.Tools` was declared for this and had **zero readers** before.

### One real gap fixed during this review

`/debug/pprof/` returned **200 with the app shell** when the profiler was gated
off. Probing whether pprof was enabled answered "yes" either way. pprof itself
was correctly gated — the SPA fallback was answering for it. Now 404s honestly.

---

## 4. Performance

**The dominant per-request cost is the tool schema block: 2,047 tokens on every
single iteration** (16 tools, 8,188 bytes). The ReAct system prompt adds 392.
So a Build turn pays ~2,439 tokens before the conversation starts, and a
25-iteration run spends ~51k tokens on tool *definitions*.

This is the highest-leverage optimisation available and nothing else is close.
Trimming tool descriptions, or offering a task-relevant subset rather than all
16, would cut more than any caching change. Read-only mode already demonstrates
the win: 476 tokens instead of 2,047.

**Polling is disciplined.** Every network poller guards on both active tab and
`document.hidden`. The two unguarded timers are local: a replay playback timer
that stops at the end, and a 100 ms elapsed-time label — the latter now guarded,
since 10 DOM writes a second for an invisible label is free to skip.

**Two fixes from earlier in this work are worth restating as performance
results**, because both were measured: the workspace code index went from a full
re-parse per request to 0.74 s cold and **0.028 s cached (26×)**; and workspace
ingestion is 2,601 embedding calls once, then **zero** on every subsequent pass.

**Goroutines: 17 at idle, stable.** Only three are DarkCode's own. No background
work consumes model bandwidth.

---

## 5. The weakest area: three surfaces, three answers

This is the finding I would escalate first, because it is structural rather than
a bug.

There are **~50 config fields**, the API exposes **~25**, the GUI renders **~20
controls**, and the CLI offers **9** setting commands — and they are *different
subsets*. Concretely:

- `plan_approval` and `plan_depth` are in config, exposed by the API, present in
  the GUI as segmented buttons, and **absent from the CLI**.
- `compress_context`, `context_length`, `max_concurrent`, `temperature`,
  `ui_mode` are in config and the API, and reachable from **neither** interface.
- `auto_ingest`, `health_daemon`, `health_cpu_percent`, `air_gap`, `deny_rules`,
  `smart_approval`, `blast_radius_threshold`, `cost_limit_*` exist in config and
  are reachable from **neither** interface and **not exposed by the API at all**.

Nothing is broken. But there is no single answer to "what can the user
configure", which means every new setting requires three separate decisions and
usually gets one or two. That is exactly how `agentic_loop` ended up existing in
the config, the API, the GUI *and* the CLI while the CLI's own command had to
apologise for it.

**The fix is not more UI.** It is to make the config type the single source of
truth and generate the other two surfaces from it — a field carries its own
metadata (label, group, whether it is advanced), the API reflects it, and both
interfaces render from that reflection. Then a new setting is one decision.

The reduction proposed in `configuration-surface.md` should land *before* that
generation, so the generated surface is six settings rather than fifty.

---

## 6. Redundancy

**Resolved during this work:** the Agentic Loop panel (a switch plus a Max Loops
field) is gone from the GUI, the CLI, the config type and `/api/config`;
`agentic_loop`, `max_loops` and `post_loop_consensus` are retired with a
non-silent deprecation note. The standalone `consensus` package — an abandoned
refactor stub that only forwarded to `router.Consensus` — is deleted.

**Remaining, in priority order:**

1. **`/mode` vs `/chatmode` vs the verbs.** Three vocabularies for "how should
   this run": `/mode` (routing), `/chatmode` (chat/build/loop), and the new
   verbs. The verbs should absorb `/chatmode` entirely.
2. **Nine undocumented CLI aliases** (`/q`, `/exit`, `/perms`, `/undo`,
   `/reset`, `/sessions`, `/projects`, `/knowledge`, `/memprofile`). Harmless,
   but they are surface nobody maintains. Keep the ones with muscle memory
   behind them, drop the rest.
3. **Nine GUI tabs, five keyboard-cyclable.** Cognition, Replay, Changes and
   Cascade are clickable but not in the cycle list — covered in
   `interface-restructure.md`.

No duplicate element IDs across pages, which is better than most UIs this size.

---

## 7. As a business analyst

**What is genuinely differentiated.** Acceptance-proved completion is the real
one — most agents stop when the model says it is done, and this one runs the
test. `Verdict.Proven()` distinguishing "criteria ran and held" from "nothing
contradicted the claim" is a distinction most products do not draw at all.
Local-first with a real knowledge graph plus a composer that reads it at
question time is the second. Both are defensible.

**Where the effort is disproportionate.** Nine tabs and ~50 settings for a
single-user local tool is a lot of surface to maintain per unit of value. The
GUI is 7,405 lines with no module system; the CLI is 4,837 lines reimplementing
what the API already serves.

**The risk that would hurt most.** Not a missing feature — the coverage
distribution. The sub-agent loop at 4.4% is where a silent behavioural
regression would live longest before anyone noticed, and it is the code most
likely to be changed next.

**Cost posture is strong** and worth saying plainly: a full benchmark run is
about two cents on a cheap model, a typical task half a cent, and the smalltalk
rung means greetings cost nothing at all. Nothing here is expensive to operate.

---

## Recommended order

1. **Test `agents` and `compression`.** Highest risk, lowest coverage, and both
   have already produced a user-visible bug.
2. **Trim the tool schema block.** 2,047 tokens per iteration is the single
   biggest cost lever in the system.
3. **Land the config reduction, then generate the surfaces from it.** Fixes the
   three-way divergence permanently rather than re-syncing by hand.
4. **Split `server.go`.** 1,108 lines across five responsibilities.
5. **Absorb `/chatmode` into the verbs**, then prune the undocumented aliases.

Items 1 and 2 are worth doing before any new feature. Item 3 is the one that
stops the same class of problem recurring.
