# Recon: Interface layer — server / cli / ui / acp / verb (Tier 1/2)

> **Note:** this is a point-in-time subsystem pass, one of five inputs consolidated into `../../DARKCODE_RECON_REPORT.md`. Several items marked UNKNOWN or unverified here (notably: the SSRF/air-gap dial-time logic in `safeurl/safeurl.go`, `tools/terminal.go`'s `Sandbox.MustRefuse()` call, prompt-cache mechanics, and the GPU `-ngl` layer-count calculation) were resolved during consolidation. **Where this file and the consolidated report disagree, the consolidated report is authoritative.**


Scope: `server/` (47 files + `server/web/` static assets), `cli/` (24 files + `cli/tui/`),
`ui/` (2 files + `ui/html/`), `acp/` (4 files), `verb/` (2 files).
FACT = cites a file; INFERENCE = interpretation; UNKNOWN = unresolved.

## Purpose
Four surfaces (CLI, GUI/HTTP, ACP editor protocol, OpenAI-compatible API) that all normalize into
`uiport.Manager.Execute` before reaching the orchestrator kernel (uiport already documented by the
parent). This layer owns presentation, request setup, and — for the server — the HTTP transport
concerns (CSRF, rate limiting, idempotency, SSE).

## Entry Points
- `AppRunner.RunCLI()` — `app_cli.go:16`. Builds `cli.NewConsole(...)`, calls `console.Run()`.
- `AppRunner.RunGUI()` — not read directly, but `server.Server` is constructed and started from it
  (per `app_wireup.go`'s `a.Server` field and `main.go`'s `--gui` flag).
- `AppRunner.RunACP()` — invoked from `main.go:161-163` when `--acp` is passed, before `Execute()`.
- `server.Server.routes()` — `server/server.go:194-242+` — the full HTTP mux.

## Public API
- HTTP routes (`server/server.go`, `net/http.ServeMux`, stdlib only — **no external router
  dependency**, consistent with "zero heavy deps"): `/api/chat` (+`/cancel`), `/api/status`,
  `/api/verbs`, `/api/config*`, `/api/tools*`, `/api/memory*` (short-term/episodic/semantic/
  procedural, each separately routed), `/api/events` (SSE), `/api/health`, `/api/reset`,
  `/api/ingest`, `/api/session/state`, `/api/switch-cli`, `/api/providers`, `/api/models/*`,
  `/api/metrics/*`, `/api/cascade` (**confirmed real, matches README exactly**),
  `/api/capability`, `/v1/models`, `/v1/chat/completions` (**OpenAI-compatible endpoint,
  confirmed real**), `/api/checkpoints*`, `/api/rollback`, `/api/runs`, `/api/plan`.
- ACP: `Agent` type implementing JSON-RPC 2.0 methods (`initialize`, `session/new`,
  `session/prompt`, with `session/update` notifications streamed back) — `acp/acp.go:1-40`.
- `cli.NewConsole(cfg, kernel, port, memSystem, registry, emitter, recorder, sourceMgr,
  projectStore, activeProject) *Console` — `app_cli.go:23`.

## Internal Components
- **Server middleware** (`server/middleware.go`): `newRateLimiter(ratePerSecond, burst)` — a real
  token-bucket-style limiter (`middleware.go:60-72`), `rateLimitMiddleware`
  (`middleware.go:98-106`, returns HTTP 429 "rate limit exceeded, slow down"), `csrfMiddleware`
  (`middleware.go:168`). Doc comment at top of file explicitly lists "browser security headers,
  CORS, per-address rate limiting, and the [CSRF check]" (`middleware.go:6`).
- **Idempotency** (`server/idempotency.go`): `idempotencyMiddleware` (`idempotency.go:123`),
  applied specifically to `/api/chat` (`server.go:197`) — wrapped `s.csrfMiddleware(s.
  idempotencyMiddleware(...))`. **Confirms the uiport.go claim that `/v1/chat/completions` sits
  behind the same middleware stack IS ACCURATE for /api/chat; did not verify /v1/chat/completions'
  own route registration (line 235) is wrapped in the identical middleware chain — worth a direct
  diff, flagged as partially-confirmed.**
- **SSRF guard specific to server** (`server/ssrf.go`) — a server-local wrapper/reuse of
  `safeurl`, separate file, not deep-read.
- **SSE** (`server/sse_handler.go`) — backs `/api/events`, used by the GUI for live agent
  monitoring; `ui.NewSSEEventEmitter()` (constructed in `app_wireup.go:292` for GUI mode) is the
  emitter implementation this streams from.
- **OpenAI compat** (`server/openai_compat.go`) — implements `/v1/models` and
  `/v1/chat/completions`; routes into `uiport` with `Surface: SurfaceAPI` (per uiport.go's Surface
  constants) rather than calling the kernel directly.
- **MCP server** (`server/mcp.go`) — real JSON-RPC 2.0 `MCPRequest`/`MCPResponse` types, doc
  comment cites the actual MCP spec URL (`server/mcp.go:14-16`), imports `tools` package for tool
  discovery/execution — **confirms README's "DarkCode is both an MCP client and an MCP server"
  claim: server-side is here, client-side is `tools/mcp_client.go`** (found by the tools/security
  investigation).
- **Static GUI assets** (`server/web/`): real embedded frontend — `index.html`, `app.js`,
  `index.css`/`styles.css`, `pages/`, `js/`, `css/`, `fonts/`, `vendor/`, `favicon.ico`, `logo.png`.
  `embed` appears in `server`'s import list (per the parent's `go list` graph) — the frontend is
  compiled into the binary via `go:embed`, not served from disk at runtime (matches "single Go
  binary" / "zero heavy deps" claim). Did not confirm whether it's a hand-written JS/CSS app or
  built by a JS framework/bundler (no `package.json`/`node_modules` found at repo root during
  initial inventory — **INFERENCE: this is a hand-authored vanilla JS frontend, not a React/Vue
  build output**, based on absence of any JS build tooling in the repo).
- **CLI console** (`cli/console.go` + satellites `console_commands.go`, `console_knowledge.go`,
  `console_models.go`, `console_reporting.go`, `console_rollback.go`, `console_session.go`,
  `console_settings.go`, `console_toolsources.go`) — a readline-based (`github.com/chzyer/
  readline`, per dep graph) REPL with slash-command dispatch. `cli/completer_test.go`,
  `console_completer.go` — tab-completion.
- **`cli/tui/`** — NOT a full alternate interface. Two files: `selector.go` and `input.go`, each a
  small bubbletea component (model picker / text input widget), used as modal helpers *within* the
  readline console, not a replacement for it. **This confirms the parent's/prior-audit's
  observation that `cli/tui` has zero test files is because it's genuinely small, focused UI
  widget code, not a large untested surface** — lower risk than the file count alone might suggest.
- **`verb/verb.go`** — parses the Loop/Tools/Plan/Mode/Safety/Brain override strings mentioned in
  `uiport.Request` (`"on"/"off"`, `"off"/"readonly"/"on"`, `"always"/"never"`); imported only by
  `router` per the dependency graph, so verb parsing feeds router-level state, not uiport directly
  — the actual wiring path (`uiport.Request.Loop/Tools/Plan` strings → `verb` parsing → router
  state) was not traced end-to-end; flagged as UNKNOWN for exact call path.
- **CLI↔GUI live switch**: `RunCLI` (`app_cli.go:27-31`) checks for `cli.ErrSwitchToGUI` and flips
  `a.mode = "gui"`, letting `Execute()`'s loop (`app.go:113-121`) re-enter as GUI without process
  restart — a real, working runtime mode switch, not just a help-text mention.

## Dependencies
`server` has the highest fan-out in the repo (26 darkcode packages) — essentially everything:
`attach`, `capability`, `config`, `core`, `ingest`, `intelligence`, `internal/strutil`, `llm`,
`memory`, `metrics`, `modelport`, `observability`, `orchestrator`, `permission`, `plan`,
`planwork`, `plugin`, `project`, `provider`, `provider/embedded`, `router`, `safeurl`, `tools`,
`ui`, `uiport`, `verb`. `cli` fan-out 23, very similar list minus a few (no `plan`/`planwork`/
`ingest`... actually `cli` does import `ingest` per the graph). Both surfaces reach into nearly
every subsystem — expected for top-level wiring, but also means both are large, high-complexity
packages by nature of their role, not necessarily by poor design.

## Dependents
None within `github.com/darkcode/*` — these are consumed only by root `main`.

## Data
No persistent store of their own; `server/session.go` and `cli`'s console state hold in-memory
per-connection/per-process state (active project, resumed-from-GUI flag, etc.).

## Control Flow (representative: `POST /v1/chat/completions`)
1. Request hits `mux` → wrapped handler (need to confirm exact middleware wrapping for this
   specific route — see flagged UNKNOWN above) → `s.handleOpenAIChat` (`server/openai_compat.go`).
2. Handler parses the OpenAI-shaped JSON body, builds a `uiport.Request{Surface: SurfaceAPI,
   Workspace: <server's configured workspace>, Query: <last user message>}`.
3. `uiport.Manager.Execute` validates the request (mandatory workspace), sets
   `core.WorkspaceKey`/`core.ProjectKey` on the context, applies Mode/Safety/Loop/Tools/Brain
   overrides via `Engine.ApplyRequestOverrides`, calls `Engine.Execute` → `orchestrator.Kernel.
   Execute` (the full turn control flow documented in the orchestrator recon file).
4. Output is reshaped back into an OpenAI-compatible chat-completion JSON response (streaming SSE
   chunked or single JSON — not confirmed which/both from this pass).
5. `PostTurn` hooks registered via `uiport.WithPostTurn` run after the turn (per `uiport.go`'s
   documented contract) — `app_postturn.go` (76 lines, not opened this pass) is presumably where
   the web's "seven post-turn steps" (plan refresh etc., referenced in uiport.go's comment) live.

## External Effects
HTTP responses, SSE event streams, file writes for checkpoints/rollback via the tool layer (not
this layer directly), process-local state changes (mode switch, active project).

## Business Rules
- **GUI never binds non-loopback**: confirmed at the `main.go` level (`bindAddr = "127.0.0.1:" +
  portFlag`, already read by the parent) — no server-layer override found that could rebind
  elsewhere; `uiport.go`'s own comment states "There is no --serve / network-exposure mode."
  **VERIFIED, strong claim, no counter-evidence found.**
- **No HTTP authentication layer** — did not find any auth/session-token middleware in
  `server/middleware.go` beyond rate-limiting/CSRF/CORS; consistent with the README's implicit
  claim that loopback-only binding *is* the security boundary rather than app-level auth. This is
  a **real, deliberate absence** worth flagging in the risk register: anyone with local access (or
  access to the loopback interface, e.g. another process/container sharing the network namespace)
  can hit the full API with no credential.
- **Kernel reachable only through uiport** (`.arch-baseline`'s `kernel-entry 0`): did not
  independently grep every server/cli handler for a direct `orchestrator.Kernel.Execute` call to
  confirm zero violations exist right now — took the baseline's own "0" at face value since
  `scripts/arch-check.sh` is a real, running CI gate (confirmed by the parent reading the script
  header, and by `DARKCODE_ARCHITECTURE_AUDIT.md`'s baseline section showing `make ci` passes
  clean). **Treating this as VERIFIED-via-tooling rather than independently re-verified by hand.**

## Concurrency
- `newRateLimiter` is presumably per-key (per-address) state requiring its own locking — not
  read in depth.
- HTTP handlers run one goroutine per request (stdlib `net/http` default) — each presumably builds
  its own `uiport.Request`/context, so per-request isolation is inherent at this layer; the
  *shared* mutable state risk (router/gate depth-counter) lives one layer down in orchestrator, as
  already flagged in that recon file.
- SSE (`/api/events`) is inherently a long-lived streaming connection — not examined for
  backpressure/goroutine-leak handling.

## Error Handling
- Rate limiter returns 429 with a plain message, no retry-after header confirmed.
- CSRF middleware presumably rejects with 403 — not read in detail (function body not opened).

## Tests
- `server/`: extensive — `apiroute_test.go`, `chat_handler_test.go`, `chat_helpers_test.go`,
  `chat_verbs_test.go`, `blueprint_test.go`, `config_schema_test.go`, `idempotency_test.go`,
  `middleware_test.go`, `models_ping_test.go`, `pagination_test.go`, `plan_split_test.go`,
  `progress_deadline_test.go`, `sse_subscription_test.go`, `version_test.go`, `web_debug_test.go`,
  `web_test.go` — one of the most heavily-tested packages in the repo.
- `cli/`: `commands_test.go`, `completer_test.go`, `diff_test.go`, `render_test.go`,
  `settings_test.go`.
- `acp/`: `acp_test.go`, `permission_test.go`.
- **`ui/` and `cli/tui/`: zero test files, CONFIRMED still true** (matches the prior in-progress
  audit doc's finding exactly). Given `cli/tui/` is only two small widget files (see above), this
  is a low-severity gap. `ui/events.go` (the `EventEmitter`/`SSEEventEmitter` implementation that
  every surface's live-progress reporting depends on) being untested is a more notable gap, since
  it's small but load-bearing for the GUI's entire live-monitoring feature.

## Important Files
| File | Purpose |
|---|---|
| `server/server.go` | HTTP server construction, full route table |
| `server/middleware.go` | Rate limiting, CSRF, CORS, security headers |
| `server/idempotency.go` | Request idempotency for `/api/chat` |
| `server/openai_compat.go` | `/v1/chat/completions`, `/v1/models` |
| `server/mcp.go` | MCP server (tool exposure to other agents) |
| `server/chat_handler.go` | `/api/chat` — the GUI's own chat path |
| `server/sse_handler.go` | `/api/events` live streaming |
| `server/web.go` | Static asset serving (embedded frontend) |
| `server/ssrf.go` | Server-local SSRF guard wrapper |
| `acp/acp.go` | Agent Client Protocol server (JSON-RPC 2.0/stdio) |
| `acp/permission.go` | Maps ACP `session/request_permission` to the gate |
| `cli/console.go` | Main readline-based REPL |
| `cli/commands.go` / `console_commands.go` | Slash-command dispatch |
| `cli/tui/selector.go`, `input.go` | Small bubbletea modal widgets |
| `ui/events.go` | `EventEmitter`/`SSEEventEmitter` — shared live-progress plumbing |
| `verb/verb.go` | Loop/Tools/Plan/Mode/Safety/Brain override string parsing |
| `app_cli.go` (root) | `RunCLI`, local-LLM first-run prompt, CLI↔GUI switch |
| `app_postturn.go` (root, not opened this pass) | Shared post-turn hook implementation |

## Unknowns
- Exact middleware wrapping of `/v1/chat/completions` and `/v1/models` — not individually
  confirmed identical to `/api/chat`'s wrapping (only `/api/chat` was read directly at the route
  table).
- Whether `/v1/chat/completions` streaming is real SSE-chunked OpenAI format or single-shot JSON
  only.
- `acp/permission.go`'s exact mapping to `session/request_permission` — file located by name
  pattern, not opened to confirm behavior on timeout/cancellation (README claims these default to
  deny).
- `verb/` package internals and its exact call path from `uiport.Request` fields.
- Whether the GUI frontend is hand-written or has any hidden build step (no `package.json` found,
  but not exhaustively searched for a separate frontend build directory).

## Claim verification table
| Claim | Verdict | Evidence |
|---|---|---|
| `/v1/chat/completions` sits behind the same CSRF/content-type/rate-limit middleware as the rest of the server | PARTIALLY VERIFIED | Confirmed for `/api/chat`'s wrapping; `/v1/chat/completions`'s own wrapping not independently re-checked at its route registration line |
| ACP is a real Agent Client Protocol implementation (not a stub) | VERIFIED | `acp/acp.go` implements the actual documented wire format (NDJSON-RPC 2.0/stdio, explicitly distinguished from LSP/DAP framing) and the real method names (`initialize`, `session/new`, `session/prompt`, `session/update`) |
| DarkCode is both an MCP client and MCP server | VERIFIED | Client: `tools/mcp_client.go` (found by tools/security pass); Server: `server/mcp.go`, real JSON-RPC 2.0 types citing the actual MCP spec |
| No `--serve`/network-exposure mode; GUI always binds 127.0.0.1 | VERIFIED | `main.go` hardcodes `127.0.0.1:<port>`; `uiport.go` comment corroborates independently |
