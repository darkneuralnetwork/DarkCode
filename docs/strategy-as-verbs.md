# Strategy is a verb, not a setting

A design for removing execution strategy from configuration entirely.

## The bug that names the problem

This is in `cli/console_settings.go` today:

```go
if c.chatMode == "loop" && !c.cfg.AgenticLoop {
    note = "(enable the Agentic Loop in /config for Loop mode to take effect)"
}
```

A user types `/chatmode loop` — a verb, at the moment they want it — and the
tool replies that it did not work, and would they please go and change a setting
first. The command surface is apologising for the configuration surface.

Two places express one intent, so a third piece of code exists to reconcile
them. That is the same shape as `ResolvedLocalMode()`, and it means the same
thing: there should be one.

## Three kinds of setting, one bucket

Config currently mixes three things that behave completely differently:

| Kind | Example | Lifetime | Who knows the answer |
|---|---|---|---|
| **Capability** | models, API keys | Install | The user |
| **Policy** | safety level, budget, air-gap | Install | The user |
| **Strategy** | loop, graph, plan depth, consensus | **One request** | **The task** |

The third row is the mistake. "Should this run iterate?" depends on whether the
task is multi-step — which the task knows and the installation cannot. Storing
it in config asks the user to predict, once and globally, something that varies
every message.

**A setting whose correct value is task-dependent should not be a setting.**

## The proposal, in three layers

### Layer 1 — Default: the user chooses nothing

The strongest version of this is not "better defaults", it is
[progressive complexity escalation](https://agentic-patterns.com/patterns/progressive-complexity-escalation/):
start at the cheapest strategy that could work and escalate only when a signal
says it is not enough. The 2026 framing is *design for escalation, not
perfection* — a failed-then-escalated step costs more than a correctly-routed
one, and far less than running everything at maximum effort.

DarkCode already has every signal this needs, and uses none of them for strategy:

| Signal | Already exists | Escalate to |
|---|---|---|
| `router.AssessComplexity` | yes | loop, when the goal is multi-step |
| Plan graph has independent nodes | yes (`Waves()`) | graph, with parallel execution |
| Loop stuck-detection (same call failing) | yes | graph — decompose instead of retrying |
| `Verdict.Proven()` false after repairs | yes | stronger model tier, or consensus |
| Plan returns a single node | yes | **de-escalate** back to loop |

That last row matters. Escalation without de-escalation ratchets: a task that
escalated once stays expensive forever. Both directions or neither.

Note what this **replaces**: the current `smart`/`auto` mode spends an LLM call
on an intent classifier *before any work happens*, with a 12-second timeout and
a deterministic fallback for when it fails. On a metered free tier that call is
pure overhead and it is the first thing to 429. Escalation costs nothing — the
signals are already computed.

### Layer 2 — Verbs override, for one message

When the automatic choice is wrong, say so at the point of use:

```
/loop   add retry logic to the HTTP client
/graph  migrate the storage layer to Postgres
/ask    how does the retry backoff work
/consensus is this migration safe
```

[Aider](https://aider.chat/docs/usage/modes.html) established this shape and it
has held up: `/code`, `/architect` and `/ask` apply to that message only.
Notably `/architect` selects a *two-model* strategy — architect proposes, editor
implements — without the user configuring any roles. The verb carries the whole
bundle.

That is the property worth copying. `/graph` should not mean "set one flag":

| Verb | What it selects | Config fields it replaces |
|---|---|---|
| `/ask` | read-only tools, no writes, no plan | `chat_mode` |
| `/loop` | iterate to acceptance, no plan gate | `agentic_loop`, `max_loops` |
| `/graph` | plan → approve → parallel waves → per-node proof → repair | `plan_depth`, `plan_approval`, `execution_profile` |
| `/consensus` | fan out to every registered model, weighted synthesis | `routing_mode`, `post_loop_consensus` |

Seven config fields become four words, and each word is meaningful at the moment
you need it rather than three screens away.

### Layer 3 — Sticky only when asked

`/mode graph` holds for the session; a bare `/graph` is one message. Default
one-shot, because a sticky mode is how people end up in the wrong one without
noticing — which is exactly what a persistent `agentic_loop: true` already is.

## The advanced parts

### Inline acceptance — the one that is nearly free

The contract system accepts arbitrary criteria and has **no user-facing surface
at all**. Criteria can only come from the planner. Exposing it:

```
/loop until `go test ./...` passes: add retry logic to the HTTP client
/loop until src/index.html exists: build the landing page
```

The user states a verifiable stop condition in the same breath as the task. The
loop then runs until that command actually passes — not until a model believes
it is finished. No configuration, no planner call, and the machinery is already
built and tested; it needs a parser and a `loop.Contract` literal.

This is the single highest-value item here. It turns "run until the task is
finished" from something the tool infers into something the user can *state*.

### Escalation must announce itself

> Started direct. Two files needed changing, so I escalated to `/graph`.

Two reasons. A silent strategy change is indistinguishable from a bug when the
cost or latency jumps. And it is how the verbs get discovered — the user learns
`/graph` exists by watching the tool reach for it, then starts using it directly
when they know the shape of a task in advance.

### The verb is also the diagnosis

When a run ends badly, the report should name the verb that would have helped:
*"acceptance still failing after two repairs — `/consensus` would bring the other
models in."* The override surface doubles as the troubleshooting guide.

### Escalation as the free classifier

Worth stating plainly because it inverts the usual tradeoff: classifier-based
routing is the standard 2026 approach and it costs a model call per turn.
Escalation gets the same routing outcome from signals that are a by-product of
doing the work. It is strictly cheaper *and* more accurate, because it reacts to
what actually happened rather than to a prediction about what might.

## What this removes

On top of the reduction in `configuration-surface.md`, these leave config
**entirely** rather than becoming advanced options:

```
agentic_loop   max_loops        plan_depth      plan_approval
routing_mode   execution_profile                post_loop_consensus
```

`max_loops` survives as an internal backstop with no user-facing knob — the
taxonomy is clear that graph execution still needs a terminal bound, but that is
a safety limit, not a preference.

Final shape: **six settings asked, ~8 advanced, strategy expressed as four
verbs.** No capability removed; a task that used to need a settings round-trip
now needs one word, and usually none.

## Sequencing

1. **`/loop`, `/graph`, `/ask`, `/consensus` as one-shot verbs**, mapping onto
   the existing `ApplyRequestOverrides` plumbing. Nothing is removed yet, so
   this is additive and safe.
2. **Delete the "enable it in /config first" coupling.** The verb becomes
   sufficient on its own. This is the bug at the top of this document.
3. **Inline `until <criterion>:`** parsed into a `loop.Contract`.
4. **Progressive escalation** wired to the signals in the Layer 1 table, with
   announcements. This is where the classifier call disappears.
5. **De-escalation**, so the ratchet does not form.
6. **Remove the seven config fields**, once the verbs have carried a release.

Steps 1–3 are additive. Step 6 is the only breaking one and comes last, by which
point nothing is reading those fields anyway.
