# Strategy and surfaces: what changed

Seven commits on `agent-execution-contract`, 40 files, +2,755/−570, 43 new tests.

They are one piece of work. The thread running through all of them: **the tool
was asking the user, or asking a model, for decisions it could take itself from
evidence it already had** — and each surface was asking differently.

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

| Signal | Move |
|---|---|
| Read-only turn needs to write | ask → direct |
| The same call keeps failing | → graph (decompose; retrying is what is failing) |
| Checks still not passing | one rung up |
| Plan came back as one task | graph → loop (**de-escalate**) |

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
| Advanced | 37 | real overrides, no default UI |
| Derived | 6 | computed from the model or the registered count |

**Redaction moved next to the field.** Each renderer used to be trusted to
remember, which is how one of them eventually does not.

**The guard:** `TestEveryConfigFieldIsDescribed` fails when a field is added
without a descriptor — verified by adding one and watching it fail.

---

## Verification

Every commit passed `gofmt` → `go vet` → `go test ./...`, with `-race` on the
touched packages. 36 packages green.

Browser-verified against a build on port 12399 (leaving the usual 12345
untouched):

- All five verbs listed; `/de` filters to `/debate`; Enter picks without
  submitting; arrows wrap; Escape leaves the text alone; click inserts.
- Hit-testing confirms the picker is genuinely on top, and it scrolls inside
  itself.
- Chat/Build and Loop absent from the DOM; the request body no longer carries
  `chat_mode`; New Chat and the Config tab still work.
- All 49 settings render, including the five previously reachable from nothing.
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

**The config-field collapse** (`configuration-surface.md` steps 1–4) — deriving
context sizes from the model, folding the efficiency booleans to always-on,
introducing `background_work`. The tier metadata now makes these mechanical, and
each one shrinks the advanced list. The structural fix that stops the divergence
recurring is in; the field-count reduction is the follow-up.

**The `document.hidden` poller work** turned out to be already done — all five
server pollers guard visibility. `240-replay`'s timer is user-driven local
playback, not a server poll, so it is left alone.

---

## One thing worth watching

`AssessComplexity` is a keyword-weight scan with a baseline of 3, and
`entryLoopComplexity` is set at 6 so a single incidental keyword cannot trip it.
That threshold is a judgement call, not a measurement. If entry routing starts
looking wrong, that constant is the first thing to look at — and unlike the
classifier it replaced, it costs nothing to re-tune and its behaviour is pinned
by a table test.
