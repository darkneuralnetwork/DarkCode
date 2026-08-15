# CodeQL triage, first full scan

The first CodeQL run over the whole tree (`security-extended`, 2026-08-15,
commit `4014645`) returned **55 open alerts**. Pull-request checks only ever
report alerts in *changed* code, so none of this was visible until the scan ran
against `main`.

Every alert is dispositioned below. Dismissals in the GitHub UI carry a
280-character comment pointing here; this file holds the reasoning.

The rule this triage follows: **a scanner that is muted broadly is worse than
none, because it reports clean and is believed.** Nothing is dismissed as a
class. Each cluster below names the specific sanitiser or the specific design
decision, and where the evidence lives.

## Summary

| Disposition | Alerts |
|---|---|
| Fixed, with a regression test that fails against the old code | 22 |
| Dismissed — sanitiser present, CodeQL cannot model it | 9 |
| Dismissed — reachable only across a documented trust boundary | 24 |

One vulnerability was found **during** this triage that CodeQL did not report:
DNS rebinding against the local server. It is the most serious thing in this
document. See "Found while triaging".

## Fixed

### project/store.go — path traversal (15 alerts: #28–#42)

**Real, and the worst of the reported set.** Every path helper in the store
funnels through `dir(id)`, which was `filepath.Join(s.root, id)` with no check
on `id`.

The ids this package *makes* are safe: `newID` is a slug plus six hex
characters and `slugify`'s charset is `[a-z0-9-]`, which contains no separator.
The ids it is *given* were never checked, and they arrive from outside —
`server/chat_handler.go:190` passes `req.Project`, straight off the request
body, into `Get`, `GetPlan` and `GetWorkflow`.

`filepath.Join` collapses `..`, so an id of `../../../../etc` resolved to
`/etc`, and `SetContext` would have written its `context.md` there: an
arbitrary-directory write with attacker-chosen content under a fixed filename.

Fixed by `safeSegment` in `dir()`, which cleans against `/` and takes the base,
so a traversal is resolved and then discarded rather than trusted. One guard
covers all fifteen sinks because they all pass through it.

Test: `project/traversal_test.go`. Against the old `dir()` it reports
`id "../../002" resolved to "/tmp/002", which is outside the store root`.

### tools/registry.go — reads on model-supplied paths (4 alerts: #2, #51–#53)

**Real, and subtle: the tool call was never the problem.** The permission gate
decides what the agent may look at, and confining reads outright would break an
approved read of a config in `$HOME`. What was wrong is where the bytes went.

- `noteFileObservation` (#2) re-reads the file and hands it to the observer,
  which `app_wireup.go` wires to `memory.ObserveFile`. An approved one-off read
  of `~/.ssh/config` became a durable belief in the knowledge graph, outliving
  the turn the user approved.
- `captureFileBefore` and the two after-reads (#51–#53) run *before* the tool
  does, so they read whatever path the model named regardless of whether the
  write was about to be refused. `write_file` and `patch` both call
  `confineWrite`, so the write never landed outside the workspace — but the
  before-snapshot was captured into the change record and rendered in the
  Changes tab. A refused write to `/etc/shadow` disclosed it.

Both now go through `withinWorkspace`, which returns the path it validated so
the read operates on the value that was checked. The snapshot policy and the
write policy are now the same policy.

Tests: `tools/observation_confinement_test.go`. Against the old code:
`a file outside the workspace was observed into the graph: …token=hunter2`.

### log-injection (3 alerts: #54–#56)

**Real, low severity, fixed rather than dismissed.** The structured logger in
`observability` is immune — it `json.Marshal`s each entry, which escapes
newlines. These three sites use stdlib `log.Printf` and interpolate values the
model controls: a tool name, a project id, an error carrying a model-supplied
path.

A forged line matters more here than in most programs, because this log is the
record of what the agent did to the user's machine. A tool named
`foo\n2026/08/15 04:00:00 [permission] user approved rm -rf /` writes a second
line that reads exactly like a real one.

Fixed with `core.LogSafe`, applied at the three sites CodeQL proved reachable.
Test: `core/logsafe_test.go`.

## Dismissed — sanitiser present, CodeQL cannot model it

These are true dataflows and false vulnerabilities. In each case the value is
constrained by a check the tool does not recognise as a barrier.

| Alerts | Location | Sanitiser | Evidence |
|---|---|---|---|
| #4, #5, #6 | `attach/attach.go:263`, `llm/client.go:562`, `provider/fetch.go:142` | `safeurl` — a dialler refusing private, loopback and link-local destinations, honouring air-gap mode. `llm/client.go:562` is literally `safeurl.EgressClient(...).Do(req)`. | `safeurl/no_bypass_test.go`, `safeurl/airgap_test.go` |
| #7, #8 | `provider/embedded/downloader.go:209, :240` | `filepath.Base` on the archive entry name before `filepath.Join(destDir, name)`. An entry cannot escape `destDir`. | read the extraction loop |
| #9, #10 | `provider/embedded/downloader.go:247, :251` | `filepath.Base` on `hdr.Linkname` too, so a symlink entry can only point at a sibling name inside `destDir`. | same loop |
| #3 | `cli/console_settings.go:48` | values come from `config.Values()`, which replaces every `Secret: true` descriptor with bullets (`config/surface.go:227`). `api_key` is `Secret: true`. | `config/surface_test.go` `TestValuesRedactsSecrets` |
| #43, #44 | `server/workspace_handlers.go:110, :131` | the handler already does the `HasPrefix(abs+sep, cwd+sep)` containment check and returns 403 before the read. | read `handleFilesRead` |

## Dismissed — across a documented trust boundary

The remaining path-injection alerts are all the same shape: **a local program,
running as the user, opening a path the user or their agent named.** That is
what this program is for. `SECURITY.md` already states the boundary: the local
web UI binds to loopback and is unauthenticated by design, and a browser tab on
the same machine is the trust boundary.

| Alerts | Location | Why it is intended |
|---|---|---|
| #45–#50 | `server/workspace_handlers.go` (`handleWorkspaceBrowse`, `handleFSBrowse`, `handleFSMkdir`, `setActiveWorkspace`) | These are the file picker and the workspace chooser. A picker that cannot browse outside the current workspace cannot attach a file from `~/Documents`, and a workspace chooser that cannot name an arbitrary directory cannot choose one. |
| #12–#16 | `attach/attach.go` | the user picks what to attach. |
| #17–#20 | `checkpoint/checkpoint.go` | paths come from the checkpoint's own manifest, written by this program. |
| #21–#23 | `ingest/ingest.go` | the user names the source to ingest. |
| #11, #24–#27 | `agents/verification_stages.go`, `loop/parse.go`, `orchestrator/acceptance.go`, `orchestrator/contract.go`, `permission/gate.go` | single sites operating on paths already inside the agent's own working set. |

**What makes this dismissal legitimate rather than convenient** is that the
boundary is enforced, not merely asserted. The server binds to `127.0.0.1`
(`main.go:81`, no flag to change it), rejects cross-origin requests, and — since
the fix below — rejects requests arriving under a non-loopback `Host`. Without
that last one this section would not have been defensible.

## Found while triaging: DNS rebinding

Not a CodeQL alert. It surfaced from asking what the eight
`server/workspace_handlers.go` alerts were actually reachable by.

`csrfMiddleware` guarded `Origin` only. Rebinding does not send a foreign
Origin — it removes the need for one:

1. `evil.com` resolves to the attacker, serves a page, and re-resolves to
   `127.0.0.1` on a short TTL.
2. The page fetches `http://evil.com:12345/api/…`.
3. The browser considers that **same-origin with itself**, so it sends no
   `Origin` header at all. `origin != ""` is false.
4. The request lands on handlers that read files, write files and run tools.

What the attacker cannot drop is `Host: evil.com`, because that is the name the
browser dialled. The middleware now requires a loopback `Host` on every path —
not just `/api/`, because a rebound page loading the UI is the first step to
driving it.

Test: `server/middleware_test.go` `TestCSRFBlocksDNSRebinding`. Against the old
middleware: `a request for Host "evil.com" reached the handler`. It also covers
a LAN address, a public IP, and `127.0.0.1.evil.com`, which prefix matching
would have waved through.

## Re-running this

```
gh api --paginate 'repos/darkneuralnetwork/DarkCode/code-scanning/alerts?state=open&per_page=100'
```

A new alert in a cluster dismissed above is **not** covered by that dismissal —
the reasoning is per-site, and a new site is a new question.
