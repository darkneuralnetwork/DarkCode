# Recon: Tools / Permission / Security subsystem (Tier 1 — highest risk surface)

> **Note:** this is a point-in-time subsystem pass, one of five inputs consolidated into `../../DARKCODE_RECON_REPORT.md`. Several items marked UNKNOWN or unverified here (notably: the SSRF/air-gap dial-time logic in `safeurl/safeurl.go`, `tools/terminal.go`'s `Sandbox.MustRefuse()` call, prompt-cache mechanics, and the GPU `-ngl` layer-count calculation) were resolved during consolidation. **Where this file and the consolidated report disagree, the consolidated report is authoritative.**


Scope: `tools/` (54 files + `tools/deterministic/`), `permission/` (12 files), `security/`
(9 files), `safeurl/` (4 files). FACT = cites a file; INFERENCE = interpretation; UNKNOWN =
unresolved. This is the security-critical layer — findings here matter most.

## Purpose
The tool registry/dispatch layer, the human-approval gate in front of dangerous actions, process
sandboxing, SSRF/air-gap network guarding, and content-safety scanning (prompt injection, secrets).

## Entry Points
- `tools.NewRegistry()` / `tools.RegisterBuiltinTools(registry, oldStore, router, sandbox,
  backend)` — `app_wireup.go:172`, called once at startup.
- `permission.Gate.Check(tool string, args map[string]interface{}) (bool, ApprovalRequest,
  string)` — `permission/gate.go:363`. The single decision point every tool call passes through.
- `security.NewSandboxForMode(mode, writable, emitter) *Sandbox` — `security/sandbox.go` (called
  once, `app_wireup.go:59`).
- `tools.NewBackend(kind, image, host, port, sandbox) (Backend, error)` — `tools/backend.go:137`.

## Public API
- `Registry.Register`, `Registry.List`, `Registry.SetSpillStore`, `Registry.SetFileObserver`.
- `Gate.Check`, `Gate.SetApprover`, `Gate.Stats`.
- `security.Scan(content string) []Finding` — prompt-injection scanner (`security/injection.go:71`).
- `security.NewSecretScanner()` / `HasSecret` / `Redact` (`security/secrets.go`).
- `safeurl.SetAirGap(bool)`, `safeurl.SafeTransport(allowLoopback bool) *http.Transport`,
  `safeurl.EgressClient(timeout)`.

## Internal Components
- **Permission gate** (`permission/gate.go`, 12 files total incl. `deny.go`, `judge.go` — an LLM-
  based auto-approve judge, `mode_approver.go`, `server_gate.go` for GUI approvals via SSE/queue).
- **Tool registry & dispatch** (`tools/registry.go`) — schema-driven tool interface;
  `tools/builtin.go` registers the core set; `tools/breaker.go` is a circuit-breaker (per-tool
  failure tracking, presumably to stop retrying a broken tool repeatedly — file exists, not
  read in depth, flagged as likely real given a dedicated `breaker_test.go`).
- **Execution backends** (`tools/backend.go`): `LocalBackend` (sandboxed via `security.Sandbox`),
  `DockerBackend`, `SSHBackend` — all three are real, fully-argv-constructing implementations, not
  stubs (see Business Rules for exact flags).
- **Path confinement** (`tools/pathguard.go`, tested by `pathguard_test.go` +
  `observation_confinement_test.go`) — the workspace-write guard referenced by `uiport.go`.
- **Sandbox** (`security/sandbox.go`) — detects `bwrap`/`firejail` via `exec.LookPath`
  (`sandbox.go:68-70`), builds a wrapped `exec.CommandContext` (`sandbox.go:174`).
- **SSRF/air-gap guard** (`safeurl/safeurl.go`) — dial-time IP validation closing a DNS-rebinding
  TOCTOU gap (see Business Rules).
- **Prompt-injection scanner** (`security/injection.go`) — 8 regex pattern classes + invisible-
  Unicode detection + homograph-URL detection.
- **Secret scanner** (`security/secrets.go`) — 12 regex patterns for AWS keys, Bearer tokens,
  GitHub/GitLab/Slack/Google/OpenAI/Stripe/Twilio tokens, PEM private keys, JWTs, AWS secret keys.
- **Deterministic tools** (`tools/deterministic/`) — real `go/ast`/`go/parser`/`go/printer`-based
  AST operations (confirmed by import list from the dependency graph, not individually enumerated).
- **MCP client** (`tools/mcp_client.go`, tested by `mcp_align_test.go`) — connects external MCP
  tool servers (stdio/HTTP), matches `main.go`'s documented `/tools connect mcp` commands.

## Dependencies
`tools` has the 2nd-highest fan-out in the repo (20 darkcode packages) — `candidate`, `checkpoint`,
`core`, `debugger`, `hooks`, `intelligence`, `internal/repowalk`, `internal/strutil`, `llm`,
`memory`, `modelport`, `permission`, `plugin`, `project`, `recall`, `safeurl`, `security`,
`selfheal`, `spill`, `ui`. `permission` imports `core`, `internal/strutil`, `security`. `security`
imports `internal/strutil`, `ui`.

## Dependents
`tools`: fan-in 8 (`orchestrator`, `server`, `cli`, `agents`, `loop`, `ingest`,
`tools/deterministic`, root `main`). `permission`: fan-in 5. `security`: imported by `config`,
`permission`, `tools`, `cli`, `server`, root `main` — a genuinely foundational package.

## Data
No persistent store of its own; the permission gate keeps in-process session state (`allowed`,
`denied` maps, counters) that resets each process run — session approvals do not survive restart
(not confirmed as a deliberate design choice vs. simply unimplemented persistence — plausibly
intentional since "allow for session" is explicitly scoped to the session).

## Control Flow — one tool call through the gate (`Gate.Check`, `gate.go:363-462`)
Verified exact ordering from source, matching (and in one respect exceeding) the README's claims:
1. **Allow-list check** (`g.notAllowed`) — refuses outright if a configured `allow_only` list
   excludes this tool. Checked first specifically so nothing downstream can step around it.
2. **Deny rules** (`g.deniedByRule`) — checked next, explicitly ahead of the relaxed fast path,
   the session cache, and the approver, "so a configured refusal cannot be overridden by how the
   session was set up or by an earlier 'allow for session'" (`gate.go:384-386`). **VERIFIED, this
   is a real, load-bearing ordering, not just a comment.**
3. **Blast-radius check** (`g.highBlastRadius`) computed *before* the relaxed fast-path check, so
   a structurally central file is escalated "even under permissive settings" (`gate.go:404-407`) —
   confirmed real, not README-only.
4. **Relaxed fast path** — approves everything *unless* `central` (blast-radius escalation).
5. **Session cache** — previously-allowed tool (and not central) passes; previously-denied
   specific call is refused.
6. **Classification** (`classify(tool, args)`) — determines risk level and whether "dangerous."
7. **Secret detection** (`argsContainSecret`) — escalates risk to at least Medium and prefixes the
   summary with a warning if a call's arguments look like they carry a credential.
8. **Central-file escalation** — forces `dangerous=true`, `RiskHigh`, and an explicit
   "N% of the repository depends on this file" message.
9. **Normal level**: non-dangerous calls pass without a prompt.
10. **Judge** (`judge.go`) — an LLM-based auto-approver ("smart_approval") may clear a call *only*
    if level is Normal, not central, and not secret-flagged — i.e. it is explicitly barred from
    clearing high-risk/secret/central cases (`gate.go:456`, "cannot clear a high/critical action
    or a secret").
11. **Ask** (`g.ask`) — prompts via the configured `Approver`. **Fail-closed confirmed**:
    `gate.go:475-476` — "No approver available... fall back to a safe default: deny in strict,
    allow otherwise" — i.e., under Strict specifically, an absent/unanswered approver denies. (For
    Normal/Relaxed levels, no-approver defaults to allow — this is a **nuance the README's blanket
    "fail-closed" framing does not capture**: fail-closed is Strict-level-specific behavior for the
    no-approver case, not universal. Flagged as a real, if minor, discrepancy.)

## External Effects
- Shell/process execution (local/docker/ssh backends).
- File writes gated by `pathguard.go` confinement.
- Network egress gated by `safeurl` (SSRF + air-gap).
- GitHub API calls (`tools/github.go`).

## Business Rules
- **Docker backend hardening** (`tools/backend.go:99-109`, `DockerBackend.Argv`): `docker run --rm
  --network <network> --cap-drop ALL --security-opt no-new-privileges --pids-limit 256 --memory
  2048m -v <workdir>:<workdir> -w <workdir> <image> bash -c <command>`. **VERIFIED**: all
  capabilities dropped, no-new-privileges, resource limits present. Did not confirm the default
  value of `<network>` is literally `none` (the variable is read from `b.Network` earlier in the
  file, not shown in the excerpt) — likely default-none per README claim but **not 100% confirmed
  from the exact excerpt read**.
- **SSH backend requires key-based auth**: `BatchMode=yes` explicitly set so a password prompt
  can't hang the agent (`backend.go:112-114`).
- **Unknown execution-backend kind is a hard error, never a silent fallback to local**
  (`backend.go:134-136`, corroborated by app_wireup.go's `MisconfiguredBackend` substitution
  already noted by the parent) — a genuinely deliberate anti-footgun design, confirmed twice
  independently (constructor + call site).
- **Air-gap + SSRF enforced at actual dial time, defeating DNS rebinding**
  (`safeurl/safeurl.go:68-96`): `safeDialControl` is a `net.Dialer.Control` hook that runs with
  the *already-resolved* IP the kernel is about to connect to — closing the TOCTOU gap where an
  earlier hostname-based check (`IsSafeFetchURL`) resolves once but the actual dial re-resolves
  later, which a hostile DNS server could flip toward `127.0.0.1`/`169.254.169.254` between the
  two lookups or across a redirect. **This is real, well-designed, security-engineering-grade
  code** — the comment explicitly names the exact attack it defeats.
- **Prompt-injection scanner is real** (`security/injection.go`): 8 regex-pattern categories
  (instruction-override, role-injection incl. `<|im_start|>`-style tokens, exfiltration via
  curl/wget grabbing token/key env vars or `.env`/`~/.ssh`, tool-coercion, pipe-to-shell), plus
  invisible-Unicode detection (zero-width space/joiners, BOM, bidi override/isolate ranges, Unicode
  tag characters used for invisible ASCII smuggling) and homograph-URL detection. Runs per-line,
  advisory only (flags, doesn't block) — confirmed against `Scan()`'s doc comment and pattern list.
- **Secret scanner covers a real, broad pattern set**: AWS access keys (`AKIA...`), Bearer tokens,
  GitHub (classic + fine-grained PAT), GitLab, Slack, Google API keys, OpenAI-style `sk-...`,
  Stripe-style `sk|rk_live|test_...`, Twilio SIDs, PEM private keys, JWTs, raw AWS secret access
  keys. 12 compiled regexes (`security/secrets.go:12-41`).

## Concurrency
- `permission.Gate` uses its own `sync.Mutex` (`g.mu`) around all counter/map state — every
  `Check` call takes and releases it multiple times across the decision chain (not one long
  critical section), reducing contention but creating small windows where two concurrent calls
  could interleave (e.g., between the allow-list read and the deny-rule check) — **not confirmed
  as an actual exploitable race**, just a structural note.
- `tools.Registry` — not confirmed whether concurrent tool registration/lookup is itself locked;
  in practice registration happens once at startup single-threaded, so this is low risk.
- `security.Sandbox`/`safeurl` guards are stateless per-call (aside from the process-wide
  `airGap atomic.Bool`), so no meaningful concurrency risk there.

## Error Handling
- Misconfigured execution backend refuses commands outright rather than silently degrading
  (already noted).
- `Gate.ask` fails closed under Strict when no approver is wired; fails open (allow) under
  Normal/Relaxed with no approver — see the nuance flagged above.
- Deny rules and allow-lists produce explicit refusal messages fed back to the agent as tool
  output (`"refused: ..."`), so the model can adapt rather than silently getting nothing.

## Tests
147 `func Test...` functions found across `tools/`, `permission/`, `security/` test files
combined — broad coverage. Notable dedicated test files: `pathguard_test.go`,
`observation_confinement_test.go`, `registry_gate_test.go`, `readonly_test.go`,
`server_gate_align_test.go`, `lock_tests_test.go`, `timeout_test.go` (approval timeout — fail-
closed behavior likely tested here), `injection_test.go`, `secrets_test.go`, `sandbox_test.go`,
`policy_test.go`, `mcp_align_test.go`. This looks like the most heavily-tested subsystem examined
so far, consistent with it being the highest-risk area.
- UNKNOWN / not confirmed: whether the docker backend's actual default `Network` value (`none` vs
  something else) is tested; whether a live sandbox/docker/ssh integration test exists (vs. only
  argv-construction unit tests) — plausible these are unit-tested only, given no docker/ssh
  daemon would be available in CI by default.

## Important Files
| File | Purpose |
|---|---|
| `permission/gate.go` | Core approval decision chain — the security spine |
| `permission/deny.go` | Deny-rule matching, checked ahead of everything permissive |
| `permission/judge.go` | LLM-based low-risk auto-approval (smart_approval) |
| `permission/server_gate.go` | GUI-side approval queue/SSE delivery |
| `permission/mode_approver.go` | Routes approval to CLI vs GUI approver by active surface |
| `security/sandbox.go` | bubblewrap/firejail process confinement |
| `security/injection.go` | Prompt-injection pattern scanner |
| `security/secrets.go` | Credential pattern scanner |
| `security/policy.go` | `.darkcode/policy.json` restriction-only enforcement |
| `safeurl/safeurl.go` | Dial-time SSRF + air-gap guard (DNS-rebinding-safe) |
| `tools/registry.go` | Tool registration/dispatch |
| `tools/backend.go` | Local/Docker/SSH execution backends |
| `tools/pathguard.go` | Workspace write confinement |
| `tools/builtin.go` | Core tool set registration |
| `tools/breaker.go` | Per-tool circuit breaker |
| `tools/mcp_client.go` | MCP server connection (client side) |
| `tools/candidate_tool.go` / `selfheal_tool.go` | Verifier-gated patch application |
| `tools/deterministic/` (pkg) | AST-based deterministic code tools |

## Unknowns
- Exact default `Network` value for the docker backend (excerpt read didn't include the variable
  assignment above line 90).
- Whether an MCP **server** (exposing DarkCode's own tools to other agents, per README) exists in
  `tools/` or lives elsewhere (e.g. `server/`) — only the MCP *client* side was confirmed here;
  flagged for the interfaces investigation.
- Depth of `tools/breaker.go`'s circuit-breaker logic (thresholds, reset behavior) — file located,
  not read.
- Whether session-scoped permission decisions (`g.allowed`/`g.denied` maps) are ever persisted
  across a GUI-mode process restart, or are purely in-memory (in-memory is the more likely design
  given no path/store field was seen on `Gate`, but not exhaustively confirmed).

## Claim verification table
| Claim | Verdict | Evidence |
|---|---|---|
| Deny rules refuse before every permissive path, can't be overridden by relaxed/session-allow | VERIFIED | `gate.go:363-402`, exact ordering confirmed in source |
| Blast-radius escalation forces approval even under permissive settings | VERIFIED | `gate.go:404-407, 440-445`, checked before the relaxed fast path |
| Sandbox really shells out to bubblewrap/firejail | VERIFIED | `security/sandbox.go:68-70` (`exec.LookPath("bwrap")`/`"firejail"`), `:174` (`exec.CommandContext`) |
| Air-gap enforced "at dial time" | VERIFIED, and more rigorously than a simple flag check | `safeurl.go:68-96`, defeats DNS-rebinding TOCTOU specifically |
| Prompt-injection scanning on reads/fetches (override phrasing, exfiltration, zero-width/bidi, homograph URLs) | VERIFIED, all four sub-claims | `security/injection.go` full pattern set matches every named sub-claim exactly |
| Secret scanning forces a prompt | PARTIALLY VERIFIED | Secret detection exists and escalates risk/adds warning (`gate.go:432-438`); "forces a prompt" is accurate under Normal level (blocks the judge from auto-clearing) — under Relaxed level a non-central secret-bearing call would still hit the relaxed fast path *unless* the secret detection itself sets `central`-equivalent force, which it does not appear to (only blast-radius sets `central`) — **this is a real, specific discrepancy worth flagging**: a secret-bearing tool call under Relaxed safety level, on a non-central file, appears to pass through the fast path at `gate.go:412` without a prompt, since that check only excludes `central`, not secret-flagged calls. **CONFIRMED via source reading, not inference** — recommend a human re-check this exact line before relying on Relaxed mode with secret-scanning as a safety net. |
| Fail-closed approvals ("unanswered prompt denies") | PARTIALLY VERIFIED | True specifically for Strict level with no/timed-out approver (`gate.go:475-476`); Normal/Relaxed with no approver default to *allow* — README's blanket phrasing overstates the Normal/Relaxed case |
| Docker backend: all capabilities dropped, no network by default | PARTIALLY VERIFIED | Cap-drop ALL confirmed; "no network by default" not confirmed from the read excerpt (network value is a variable, source of its default not traced) |
