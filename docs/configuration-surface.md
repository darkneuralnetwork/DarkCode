# Reducing what the user has to decide

DarkCode's config has **53 top-level fields**. Most of them are questions the
tool is better placed to answer than the person using it. This is an audit of
which ones are real decisions, which are derivable, and which are the same
decision asked twice.

The goal is not fewer features. Every behaviour below stays. The goal is that
the number of things you must have an opinion about before the tool works
should be small, and everything else should have a right answer the tool picks
until you say otherwise.

## What a new user must currently get right

Nothing works without a model, so that is the floor. But the floor is wider than
it needs to be: `model`, `provider`, `base_url` and `api_key` exist at the top
level *and* the same four fields exist inside every entry of the `models` map.
The top-level set is kept for backward compatibility. So the first thing a new
user meets is two different ways to say the same thing, with no indication which
one wins.

## The same decision, asked more than once

### Local model: four fields, one question

```
enable_local_llm          bool
local_mode                "off" | "auto" | "on" | "force"
enable_local_offloading   bool
local_model_role          string
```

`local_mode` already expresses everything `enable_local_llm` does. The proof is
that the code has to reconcile them — `Config.ResolvedLocalMode()` exists purely
to decide what to do when they disagree, mapping `enable_local_llm: true` onto
`"auto"` when `local_mode` is empty. A function whose entire job is to resolve a
contradiction between two settings is the clearest possible sign there should be
one setting.

**Reduce to:** `local_mode`. Keep `enable_local_llm` readable for old configs,
stop writing it, and drop it from every UI.

### Routing mode is mostly a consequence, not a choice

`routing_mode` is `single` | `escalation` | `consensus`. But consensus with one
registered model already falls back to primary-only, and escalation only means
anything when more than one tier is registered. So with a single model the
answer is forced, and the user is being asked a question with one valid answer.

**Reduce to:** derive the default from how many models are registered — one
model means single, several means escalation — and keep the field as an explicit
override for the case that actually needs it (consensus).

### Five booleans that all mean "be efficient"

```
compress_context        use_ctx_engine        use_local_for_aux
skip_aux_for_read_only  post_loop_consensus
```

Four of these make the tool cheaper or faster with no behavioural downside, and
nobody deliberately turns them off. `post_loop_consensus` is the exception —
it costs extra calls for polish — and it is already off by default.

**Reduce to:** delete the first four as user-facing settings and have them
always on. Keep `post_loop_consensus`, since it genuinely trades money for
quality.

### Three fields for "do background work"

```
health_daemon        health_cpu_percent      auto_ingest
```

These are one preference — whether DarkCode may use idle capacity to keep its
own indexes current — split across three switches, one of which is a percentage.

**Reduce to:** one `background_work` setting (`off` | `light` | `full`) that
sets all three. The CPU percentage stays reachable for anyone who wants it, but
stops being something you have to think about.

### Budgets that should adapt

```
max_turns    max_loops    max_concurrent    context_length    embedded_context_size
```

`context_length` and `embedded_context_size` are properties of the model, not
preferences — the router already knows the model's window. `max_turns`,
`max_loops` and `max_concurrent` are safety ceilings whose right value depends
on the task, and now that acceptance criteria decide completion, the ceilings
are backstops rather than the thing steering the run.

**Reduce to:** derive the context sizes from the model. Keep the three ceilings
but move them out of the main surface — they are for someone diagnosing a
runaway, not for someone starting out.

### Safety is one posture expressed six ways

```
safety_level    smart_approval    approval_timeout_seconds
deny_rules      blast_radius_threshold    plan_approval
```

`safety_level` is the real decision. The other five are the dials that
*implement* a posture, and asking for them separately means a user can pick
"strict" and then accidentally undermine it.

**Reduce to:** make `safety_level` set sensible values for the rest, and treat
the individual dials as overrides for people who know they want one.

## Proposed shape

Six things a user is asked, in this order:

| Setting | Why it stays |
|---|---|
| `models` | Nothing works without one. Irreducible. |
| `safety_level` | How much autonomy to grant is genuinely personal. |
| `local_mode` | Whether to run offline is a real constraint, not a preference. |
| `cost_limit_*` | Only the user knows their budget. |
| `background_work` | Whether the tool may use idle capacity on their machine. |
| `air_gap` | A hard requirement in some environments; cannot be inferred. |

Everything else becomes: derived from the model, implied by `safety_level`,
always-on because there is no reason to turn it off, or an advanced override
that exists in the file but appears in no default UI.

That takes the surface from 53 fields to **6 asked, ~15 advanced, the rest
derived** — without removing a single capability.

## Why the GUI already looks simpler than this

The Settings tab renders 16 controls across 9 sections, so the browser already
hides most of the 53. That is good, but it means the config file, the CLI slash
commands and the GUI disagree about what the product's surface *is* — and the
two that are harder to discover are the wider ones. The reduction above should
land in the config type itself, so all three surfaces narrow together rather
than each hiding a different subset.

## Sequencing

1. **Stop writing the redundant fields.** Keep reading them so existing configs
   load unchanged; write only the canonical one. Zero user impact.
2. **Derive what is derivable** — context sizes from the model, routing mode
   from the registered count. Existing explicit values still win.
3. **Collapse the efficiency booleans** to always-on and delete them from the
   config type.
4. **Introduce `background_work`**, mapping the three existing fields onto it.
5. **Split the UI** into the six settings above plus an "Advanced" section, so
   the default view matches the real decision count.

Steps 1 and 2 are backward compatible by construction. Step 3 is the only one
that removes fields, and only ones whose non-default value nobody wants.
