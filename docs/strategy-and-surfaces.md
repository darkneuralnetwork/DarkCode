# Strategy and surfaces: what changed

Twenty-eight commits on `agent-execution-contract`, 87 files, +6,831/−636,
179 new tests.

Two bodies of work with a shared cause.

**Part one (§1–8)** replaced decisions the tool was asking for — of the user, or
of a model — with decisions it could take itself from evidence it already had.
Each surface was asking differently, so the same intent had three spellings.

**Part two (§9)** is what a re-review turned up afterwards. Eleven defects, and
after the third it stopped being a list and became a pattern: *a feature built
correctly, wired incompletely, with no test crossing the seam.* In every case
the component was right and the join was wrong.

---

## 1. A conflict between models triggers one round of debate

`ce225ac` · `orchestrator/debate.go`, `orchestrator/consensus.go`

Consensus adjudication verifies each candidate answer's checkable claims against
the knowledge graph and keeps the best-supported one. When nothing in the
answers was checkable, that path had nothing to work with, so it returned the
synthesis and moved on — the one case where the models disagreeing is all the
information there is.

That case now gets a single round: each side critiques the other, then a judge
settles it.

**Why one round.** The research is unusually consistent: accuracy plateaus at
two or three rounds and two to four agents, debate frequently fails to beat
plain self-consistency at equal token cost, and unstructured rounds lose
accuracy to problem drift. The published mitigations are a judge or abort, and
an external anchor — so the original question is re-pinned in every critique
prompt, and the exchange ends at a judge rather than looping.

**Why it is gated, not a mode.** Intrinsic self-critique does not reliably
improve reasoning; externally grounded feedback does. This codebase already has
the grounded version in two places — `repair.go` feeds a failing command's real
output back into the loop, and `consensus.go` checks claims against the graph.
For anything machine-checkable, running the check beats any amount of model
conversation at one call instead of N×R. Debate is strictly the fallback for
when the grounded path cannot reach.

On a free tier metered at twenty requests a day, an unconditional three-model
three-round debate is a nine-times multiplier — two questions and the day is
gone.

The exchange is published to the agent bus, which had carried a
`MsgCritiqueRequest` kind since it was written and never sent one.

**Proof it is gated:** removing the `supports[0].Checked == 0` condition makes
`TestGroundedCheckBeatsDebate` spend 3 calls where it should spend 0.

---

## 2. The verbs reach the browser

`154d07a` · new package `verb/`, `server/chat_handler.go`, `server/web/js/75-verbs.js`

The strategy verbs shipped inside the console package. So `/loop fix the parser`
was understood in the terminal and **sent to the model as literal text in the
browser** — one intent with a console spelling and no web spelling at all.
Meanwhile the composer had advertised "`/` for Commands" since it was written,
with nothing behind it.

- The table moved to its own package that both surfaces read.
- `/api/verbs` serves it; the browser renders a picker rather than keeping a
  second copy of the list, which is how the two drifted in the first place.
- The chat handler strips a leading verb **before anything else reads the
  query**, so the classifier saw the task rather than the instruction about how
  to run it — and skipped its call entirely, since an explicit verb is a
  decision already made.
- `/debate` added.

**Caught by a test:** `/debate` and `/consensus` initially selected identical
strategies, because the distinctness key did not include the new field. The same
test had already caught `/graph` being a synonym for `/loop`.

---

## 3. The composer stops asking up front

`55b566f` · `server/web/pages/nexus.html`, `server/web/js/*`

The Chat/Build segment and the Loop toggle both asked the user to predict, once
and in advance, something that changes every message — the same mistake the
`agentic_loop` setting made, moved into the toolbar. With the verbs reaching the
browser there were two ways to say the same thing, and the toolbar was the worse
one: it was sticky, so a wrong choice persisted silently.

Requests no longer carry `chat_mode` at all. Verified by intercepting a live
send: the body is now `query`, `project`, `brain`, `attachments`.

The surrounding plumbing went too — the mode picker's loop entry was gated on a
master setting that no longer exists, and the writes to `#chat-mode-btn` had
outlived the element itself.

---

## 4. Routing by escalation, not by prediction

`a7c239f` · new `router/escalate.go`, `orchestrator/escalate.go`

**The classifier is gone.** It spent a model call before any work started,
guessing how hard the request would be. It carried a 12-second timeout, was the
first thing a metered tier rate-limited, and **fell back to a keyword scan when
it failed** — which is the tell: the keyword scan was already carrying it in
exactly the conditions that mattered. That scan is now the entry point.
`extractJSONObject` went with it, since it existed only to unwrap the
classifier's fenced JSON.

Net for that commit: 51 lines added, 175 removed in the handler — and one LLM
call off every single request.

The run then climbs on evidence:

| Signal | Move | Wired? |
|---|---|---|
| The same call keeps failing | → graph (decompose; retrying is what is failing) | **yes** |
| Plan came back as one task | graph → loop (**de-escalate**) | **yes** |
| Checks still not passing | one rung up | ladder only — nothing emits it |
| Read-only turn needs to write | ask → direct | ladder only — and `/ask` must not start writing |

The last two rungs exist and are tested but **no production code fires them**.
`SignalUnproven` is wireable — `loopRes.Verdict` is right there at the call site
— but climbing on a failed acceptance check spends real money, so it is a
decision rather than an oversight to leave open. `SignalNeedsTools` is arguably
one that should never fire: `/ask` is a promise not to change anything, and
silently upgrading it to a writing turn breaks that promise.

**The ladder runs downward too.** Escalation alone ratchets — one climb and the
run stays expensive to the end. The de-escalation test catches exactly this:
with that branch removed, the path spends 3 calls instead of 1.

**Every move is announced**, naming the verb that selects it directly. A silent
strategy change is indistinguishable from a bug when the cost or latency jumps,
and watching the tool reach for `/graph` is how someone learns to type it.

The default rung stays quiet. My first version announced every message, which
would have made this the most frequent event in the feed and the first one
anybody learned to skip — and there is no verb to teach for the rung you get by
typing nothing.

**Proof both paths bite:** disabling escalation fails `TestStuckLoopEscalatesToAGraph`;
disabling only the de-escalation branch fails `TestSingleNodePlanDeEscalates`
with a 3-vs-1 call count.

---

## 5. The console speaks the same vocabulary

`5e30ec1` · `cli/*`, `verb/verb.go`

The console kept its own chat/build/loop modes alongside the routing mode and
the verbs — three vocabularies for "how should this run", which is how it ended
up able to disagree with itself. Picking Loop printed a note telling you to go
and enable it somewhere else.

`/always` now takes a verb and nothing else, `/chatmode` is gone, and a message
with neither falls through to the same escalation the browser uses.
`verb.ForEffort` is the single mapping from rung to strategy, so an escalation
to loop and a typed `/loop` cannot mean different things — otherwise "same as
/loop" in an announcement is a lie.

**The nine aliases the switch quietly accepted are listed, not removed.**
Deleting them would have broken muscle memory for no gain — `/q` and `/exit` are
universal, `/undo` reads better than `/rollback`. The problem was never that
they existed; it was that nothing named them.

**A test I had to fix.** My first alias test compared the alias table against a
list *derived from the alias table* — self-consistent by construction, and it
passed for an alias the console had never heard of. The real version parses the
command switch with `go/ast`, and fails on a bogus alias.

---

## 6. One answer to "what can I configure"

`02b4ed1`, `54b6c97` · new `config/surface.go`, `server/config_schema.go`,
`server/web/js/135-cfgsurface.js`

There were **four** answers. The config type carried 49 fields, the API exposed
19, the Settings tab rendered 16, the console printed 11 — different subsets
each time:

- `plan_depth` reached the browser but not the console.
- `air_gap`, `deny_rules`, `blast_radius_threshold` and both cost limits reached
  **neither**, and were not in the API at all.
- `temperature` and `max_concurrent` were settable over HTTP and invisible
  everywhere.

Nothing was broken by this, which is why it lasted. The cost was that a new
setting needed three decisions and usually got one — `agentic_loop` ended up in
all four places, with the console's own command printing an apology for the
config field.

Each field now carries its label, group and tier next to itself. The surfaces
render from that: `/api/config/schema` for the browser, `/config` for the
console. Current values ride along with the schema so it is one call.

**The tiers are the reduction** — no capability removed, the default view just
stops matching the field count:

| Tier | Count | Meaning |
|---|---|---|
| Asked | 6 | models, safety, local mode, spend cap, air gap, background work |
| Advanced | 36 | real overrides, no default UI |
| Derived | 8 | computed from the model or superseded by a canonical field |

**Redaction moved next to the field.** Each renderer used to be trusted to
remember, which is how one of them eventually does not.

**The guard:** `TestEveryConfigFieldIsDescribed` fails when a field is added
without a descriptor — verified by adding one and watching it fail.

---

## 7. One question where three fields were asking it

`e261a9d` · new `config/canonical.go`

`enable_local_llm` and `local_mode` both said whether to run a local model. The
proof that this is redundant rather than merely verbose is that
`ResolvedLocalMode()` exists — a function whose entire job is deciding what to
do when the two disagree. Likewise `health_daemon`, `health_cpu_percent` and
`auto_ingest` were three switches for one preference.

The fix is deliberately asymmetric. Every legacy field is still **read**, so an
existing config keeps exactly the behaviour it had. Only the canonical field is
**written**, so a config that gets saved stops carrying the contradiction
forward. The redundancy drains out of real files over time instead of needing a
migration.

`background_work` (off / light / full) is the new primary setting, reachable
from both surfaces — it had a control in neither before.

**Two things I got wrong first.**

The saved form is produced from a *copy*. My first version zeroed the legacy
fields on the live config, which would have flipped `auto_ingest` to false in
the running process the moment any unrelated setting was saved — a behaviour
change from an operation meant to be a no-op.

And `Values` now reports what is *in effect*, not what is stored. The raw struct
misled in both directions: `background_work` marshalled away under `omitempty`
when it was being inferred, so a primary setting rendered as unset while
actually resolving to `full`; and the superseded legacy fields kept their old
values, so the derived rows contradicted the canonical one directly above them.
I only caught the second by reading the rendered panel and noticing it disagreed
with itself.

**Proof it is safe:** a legacy config file round-trips through save-and-reload
with every resolver returning the same answer, checked both by
`TestCanonicalRoundTripPreservesBehaviour` and by hand against a real file.

---

## 8. A guess standing in for data

`95608ea` · new `llm/window.go`, `config/providers.go`

Chasing "derive context sizes from the model" turned up a real defect rather
than a refactor. `ModelInfo` answered *how much context does this model have*
with a hardcoded 8,000, raised to 32,000 if the name contained `32k` or
`claude-3`. Measured across the models anyone would actually run:

| model | reported | actual | out by |
|---|---|---|---|
| gemini-2.5-flash | 8,000 | 1,048,576 | 131× |
| claude-sonnet-4-5 | 8,000 | 200,000 | 25× |
| gpt-4o | 8,000 | 128,000 | 16× |
| claude-3-5-sonnet | 32,000 | 200,000 | 6× |

The compression trigger fires at 60% of that figure, so it was firing at ~4,800
tokens on a million-token window — spending a model call to discard context that
would have fit, on every long conversation.

**The fix was mostly deletion.** `config/providers.go` already carried the
window per model, curated next to the pricing. It simply had no accessor that
searched across providers, so a caller holding only a model id could not reach
it and guessed instead. A pattern table backs it up for what the catalogue does
not list — self-hosted builds, models newer than the catalogue, and the dated or
vendor-prefixed ids (`claude-haiku-4-5-20251001`, `openai/gpt-4o`) that exact
matching misses.

I nearly shipped the pattern table *instead of* using the catalogue, which would
have been a second copy of a curated list — the exact failure the rest of this
work was spent removing. The test caught the precedence when the catalogue's
128,000 for `mistral-large-latest` disagreed with my pattern's 131,072.

Unknown models return 0 — "I don't know" — rather than a number either table
invented, and the caller falls back to the configured `context_length`.
Under-reporting only wastes a call; over-reporting gets the request rejected, so
both tables are conservative wherever a family's members differ.

---

## 9. Eleven defects, one shape

A fresh bottom-to-top review after the work above, then a targeted hunt once
the pattern became clear. Listed by what a user would have experienced.

### Silent, and costing money or safety

**The spend cap was a starting gun.** The cost governor was consulted once, at
the top of `Execute`. A single request then makes up to twenty-five acting
turns plus planning, consensus fan-out and sub-agent calls — so a cap was
checked once and could be exceeded several times over inside the run it was
meant to bound. The loop now consults the same governor between turns, and the
stop reason stays distinct from running out of iterations: reporting a spend cap
as "max iterations" sends someone to change the wrong setting.

**Nothing was priced.** The provider catalogue's newest Anthropic entry was
Claude 3.5, so the pricing lookup found nothing for any current model, cost
recorded as zero, and the cap never fired regardless. A limit with no prices to
work from is a limit that cannot trigger.

**Air gap was not airtight.** It is enforced in the dialer's `Control` hook, so
it only covers clients `safeurl` hands out — and nine places built their own,
including the model downloader pulling GGUF files from HuggingFace, the MCP
client, and the provider ping sitting in the same file as a chat path that used
the guarded client correctly. A tree-scanning test now fails on any reintroduced
raw client, with file and line.

**The execution backend lied.** `NewBackend` deliberately errors on an unknown
or incomplete backend rather than defaulting to local — its own comment names
"I thought it ran in Docker" as the misunderstanding the feature exists to
prevent. The caller fell back to local anyway, warning on a stderr that in GUI
mode nobody reads. The terminal tool now refuses and says why.

### Silent, and degrading trust

**An unanswered prompt killed a tool.** An approval that timed out was cached
exactly like a refusal the user gave. Step away once and that tool was dead for
the rest of the run — every later call denied instantly, never asking again. A
timeout is the absence of an answer.

**A refusal was read too widely.** Denying `write_file` for `/etc/passwd`
blocked every `write_file` for the session. The allow side already distinguishes
once from session; the deny side took the widest reading of a narrower answer.
Now keyed on the whole call.

**Compression dropped the messages that mattered most.** Every error indicator
required a colon — `error:`, `failed:`, `panic:`. But `exit status 1` is what a
failed `go build` reports and what the repair loop feeds back, and it scored
zero. So failure messages were the likeliest to be compressed away, leaving the
model reasoning about an error it could no longer see.

**Every model reported an 8,000-token window.** `ModelInfo` guessed, raising to
32,000 only for names containing `32k` or `claude-3`. Gemini 2.5 Flash has
1,048,576 — out by 131×. Compression fired at 60% of a figure that was mostly
imaginary. The catalogue already held the real number; it had no accessor a
caller with only a model id could reach, so the code guessed instead of looking.

### Crashes and resource exhaustion

**A malformed workspace file could kill the process.** The file watcher's
callback parses whatever turned up, on a bare goroutine, where a panic cannot be
recovered by whoever started it. The ingest half of that chain was guarded; the
parse half was not. The test crashes the binary without the guard rather than
failing.

**Two unbounded response reads.** `FetchURL` has always capped at 500KB;
`searchGitHub` and `searchWikipedia`, same file and same threat model, read
whatever arrived.

### And one in the write-up

The escalation table in this document listed four signals as working. Two of
them are ladder-only — nothing in production emits them. Corrected in §4 rather
than quietly wired, because one spends money and one probably should never fire.

### What did not turn out to be a defect

Worth recording, because verifying cost a minute each and filing them would have
been worse than useless:

- The smart-approval judge's comment claims it cannot clear a high or critical
  action while the call site only checks blast radius and secrets. The check is
  real; it lives in `judgeAllows`.
- The SPA fallback looked able to answer `/api/` paths. It cannot — there is a
  test proving unrouted `/api/` returns a JSON 404. I was grepping the wrong
  file.
- Attachments use a raw HTTP client, which looked unguarded. They call
  `IsSafeFetchURL` first, which does check air gap.
- Rollback looked like it would abort when asked to delete a file the user had
  already removed. `Diff` compares against the current workspace, so the file is
  never listed as created. The design is robust there.

---

## Verification

Every commit passed `gofmt` → `go vet` → `go test ./...`, with `-race` on the
touched packages. 38 packages green, 6 with no tests (all either off by default,
type declarations, or needing a live terminal).

**Every fix in §9 was verified by reverting it** and confirming the new test
fails against the old code — not because the test passes, which proves nothing.
Three are worth naming:

- Removing the guard makes the watcher test *crash the binary* rather than fail.
- A naive double-quote `shellQuote` lets `$HOME` through as `/home/kali` and
  executes backticks, against a real bash rather than an assertion about one.
- Restoring the tool-scoped refusal makes the approval test report the approver
  consulted once where it must be consulted twice.

Coverage moved where it mattered rather than everywhere:

| Package | Before | After |
|---|---|---|
| `safeurl` | 71.2% | **91.5%** |
| `permission` | 55.5% | 59.2% |
| `attach` | **0%** | 45.6% |
| `plugin` | **0%** | 41.2% |
| `scheduler` | 15.9% | 40.7% |
| `compression` | 22.9% | 37.6% |
| `cli` | **1.4%** | 10.6% |

`cli` needed an unblock before it could be tested at all: every setter ends in
`Save`, and `ConfigPath` resolved to the developer's own `~/.darkcode/config.json`
with no override. `DARKCODE_CONFIG` now wins outright.

Browser-verified against a build on port 12399 (leaving the usual 12345
untouched):

- All five verbs listed; `/de` filters to `/debate`; Enter picks without
  submitting; arrows wrap; Escape leaves the text alone; click inserts.
- Hit-testing confirms the picker is genuinely on top, and it scrolls inside
  itself.
- Chat/Build and Loop absent from the DOM; the request body no longer carries
  `chat_mode`; New Chat and the Config tab still work.
- All 50 settings render, including the five previously reachable from nothing.
- `background_work` set to each level over the API; invalid levels rejected with
  400 and no change; the derived rows follow instead of contradicting.
- No horizontal body overflow at 1280 / 768 / 375.
- No console errors at any point.

**Not verified end-to-end against a live model.** Doing so would spend Gemini
free-tier requests (20/day). The logic is covered by unit tests and the
event-rendering path was checked with a synthetic event.

---

## Deliberately not done

**ES-module conversion of the GUI** (`interface-restructure.md` phase 2).
Seven thousand lines of JS whose load order *is* the module system. It is a
multi-session refactor with real regression risk and no user-visible benefit,
and it is not something to attempt blind at the end of a long session.

**Tab consolidation, shared poller, CLI-onto-HTTP** (phases 4–6). The doc itself
puts these after 1–3.

**The efficiency booleans** (`configuration-surface.md` step 3). The doc says
`compress_context`, `use_ctx_engine`, `use_local_for_aux` and
`skip_aux_for_read_only` should all become always-on with "no behavioural
downside". Three of those are fine. `use_ctx_engine` is not: its own comment
says *"Default false (raw STM append) to preserve behavior"*, so flipping it on
is a real behaviour change, not a cleanup. I left all four alone rather than
flip three and leave the interesting one — that decision wants a live comparison
I cannot run without spending quota.

**Collapsing `context_length` out of the config entirely.** It is now a fallback
rather than the primary source (see §8), which is the useful half. Removing the
field would leave nothing to fall back to when a model is unrecognised.

**The `document.hidden` poller work** turned out to be already done — all five
server pollers guard visibility. `240-replay`'s timer is user-driven local
playback, not a server poll, so it is left alone.

---

## 10. Taking two verbs back out

`/consensus` and `/debate` shipped with the other three and were removed again
after the surfaces were rebuilt around them. Worth recording, because the
mistake is subtler than the eleven in §9 and it was mine.

The rule the verbs are built on is that strategy belongs at the point of use:
whether *this* request should iterate depends on the request, so it cannot be
configuration. That rule is right, and I applied it one step too far. Consensus
does not change how the work is done — it changes **how many models do it**, and
that is a property of the installation: which models are registered, whether the
tier is metered, whether the user wants every answer cross-checked. It does not
vary message to message the way "is this multi-step" does.

The concrete failure the user hit: with fan-out expressible in both places, the
sticky one wins silently. Set consensus in the configuration tab, type an
ordinary message, and every query fans out — while the chat surface advertises
`/consensus` as though it were the control. A verb that quietly loses to a
setting is worse than no verb, because it teaches the wrong model of what
governs a run. The verbs exist to kill exactly that ambiguity, so a verb that
recreates it is self-defeating.

So `routing_mode` came back to the configuration tab (`ca66349` reverts its
removal), and the two verbs came out of the table. What is left in `verb.Names()`
is only what genuinely varies per message: `/ask`, `/loop`, `/graph`.

Removing `/debate` left debate itself unreachable — it had never had a config
field, only a runtime `SetDebate` the verb called. So it became one: a `debate`
bool, wired at startup in `app_wireup.go`, exposed as a checkbox beneath routing
mode, and meaningful only in consensus mode. Along with it went
`ApplyDebateOverride` and `Strategy.Debate`, which nothing could reach any more —
a per-request override with no caller is a branch that reads as a feature.

The invariant is now pinned rather than remembered. `TestNoVerbChangesRoutingMode`
walks the table and fails on any verb that sets `Mode`, so the next person
tempted to add `/consensus` back finds out at test time and reads why.

---

## One thing worth watching

`AssessComplexity` is a keyword-weight scan with a baseline of 3, and
`entryLoopComplexity` is set at 6 so a single incidental keyword cannot trip it.
That threshold is a judgement call, not a measurement. If entry routing starts
looking wrong, that constant is the first thing to look at — and unlike the
classifier it replaced, it costs nothing to re-tune and its behaviour is pinned
by a table test.
