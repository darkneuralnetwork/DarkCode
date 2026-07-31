# Debate, roles and hierarchy

A design for `/debate`, for what a role should actually mean, and for whether
hierarchy is worth building. Nothing here is implemented yet.

Three things in the tree are built and unreachable, and all three matter to this
design. Verified, not assumed:

| Thing | State | Evidence |
|---|---|---|
| `agents/bus.go` | Constructed at `kernel.go:229`, stored at `:252`, **never sends or receives** | no `Publish`/`Send` call site outside the file |
| `result.Conflict` | Set at `router.go:711/714`, emitted to the UI at `:725` | **no decision logic reads it** |
| `SubAgentConfig.Tools` | Declared at `core/orchestrator_types.go:243` | **zero readers anywhere** |

The last one is a live security hole, not a tidiness issue. `agents/subagent.go`
hands every sub-agent the entire registry, so a `research` agent summarising a
fetched web page has shell execute and file write. That is a prompt-injection
path: untrusted page content reaches a model that can run commands.

## 1. `/debate` — conflict-triggered, one round, evidence-terminated

### Why not the obvious version

N models, R rounds of back-and-forth, vote at the end. The literature is
unusually consistent that this underperforms its cost:

- Accuracy plateaus at **2–3 rounds and 2–4 agents**, and debate frequently
  fails to beat plain self-consistency at equal token spend.
- Unstructured rounds lose accuracy to **problem drift** — agents wander off the
  original question and degradation compounds per round. The published fixes are
  a judge/abort and an external anchor.
- **Intrinsic self-critique does not reliably improve reasoning.** Externally
  grounded feedback does: execution results, test output, a real verifier.

That last point is decisive here, because DarkCode already has the thing debate
is a weak substitute for. `orchestrator/repair.go` takes a failing acceptance
check and feeds the compiler's actual output back into the loop.
`orchestrator/consensus.go` verifies each candidate's claims against the
knowledge graph. **For anything machine-checkable, running the test beats any
amount of model conversation** — and costs one call rather than N×R.

There is also a hard local constraint. On a 20-request/day free tier, an
unconditional 3-model × 3-round debate is a **9× quota multiplier** — two
questions and the day is gone. Gating is not merely elegant, it is the only way
this is usable at all.

### The version worth building

Debate is the **fallback for the case evidence cannot settle**, not a mode.

```
conflict detected (result.Conflict)
        │
        ├─ AdjudicateCandidates finds checkable claims → verify against the KG, done
        │
        └─ nothing checkable (supports[0].Checked == 0)   ← today this branch shrugs
                   │
                   └─ ONE round of mutual critique over the AgentBus, then synthesise
```

Concretely:

1. **Trigger:** `result.Conflict` is already computed and currently only
   decorates the UI. Make it load-bearing.
2. **Gate:** only when `AdjudicateCandidates` finds nothing machine-checkable.
   If the graph or a test can settle it, that path is strictly better.
3. **Exchange:** send the two disagreeing contributions to each other as
   `MsgCritiqueRequest` over the existing bus. **One round.**
4. **Anchor:** re-pin the original question in every critique prompt. This is
   the published mitigation for problem drift, and it is the same failure that
   made swarm/blackboard not worth building.
5. **Terminate:** synthesise. No second round, no vote.

That captures nearly all the measured benefit, caps drift at one round, spends
extra tokens only on the small fraction of queries where models actually
disagree *and* the graph cannot adjudicate — and finally makes both the bus and
the `Conflict` flag do something.

### `/debate` as an explicit verb

Per the verbs design, `/debate <question>` forces the exchange for one message
regardless of whether conflict fired — for when the user knows the question is
contested and wants the disagreement surfaced. Still one round. Still anchored.

Self-debate with a single model is the degenerate case: the same model answers
twice under two role personas, then critiques across. Worth supporting so the
verb works on a single-model install, but it is the weaker form — the research
above is specifically that *intrinsic* critique underdelivers.

## 2. Roles: keep them, and make them mean something

The six personas (`critic`, `skeptic`, `verifier`, `analyst`, `creative`,
`knowledge_booster`) are worth keeping as-is. They are cheap, they are already
role-weighted by reliability, and `RoleSelector` already picks a subset by task
type.

What is missing is **authority**. A role today affects exactly two things: the
system prompt and the model tier. No role can constrain another, and
`RoleExecutive` is never spawned at all — it exists only as an audit label.

### Tool scope is the hierarchy

The genuine hierarchy axis is not seniority-as-flavour-text, it is *what you are
allowed to do*:

| Role | Should get | Has today |
|---|---|---|
| `research` | read-only (`LLMSchemasReadOnly` already exists) | full write + shell |
| `critic`, `qa`, `security` | read-only + report | full write + shell |
| `worker` | read + write, no deploy | full |
| `ops` | full, including execute | full |

Wiring `SubAgentConfig.Tools` through `Spawn` is a small change that produces a
real hierarchy — level determines capability, not just tone — and it closes the
injection path independently of anything to do with debate. **This is the
highest-value item in this document and the cheapest.**

### Skip personality traits

Adding "thorough, cautious, assertive" as adjectives is not worth it:

- Trait effects are strongly task-dependent; in coding tasks specifically, low
  agreeableness shifted communication a lot and milestone completion barely.
- Persona expression is **unstable under trivial prompt rewording**.
- Persona conditioning can **override explicit incentives** — which here means a
  trait adjective quietly outranking an acceptance criterion. That is
  unacceptable in a system whose entire completion story is now
  evidence-based.

"Team lead", "pentester", "manager" are worth having *as roles with tool scope
and escalation rights*. Make seniority a scalar mapping to tier, whether output
requires review, and who it may escalate to. Deterministic and testable, with no
personality vocabulary.

## 3. Advisor — yes, renamed, and after the gate

Worth adding, with two conditions.

**Run it after acceptance passes, never instead, never blocking.** Its job is
"this is correct, here is how it could be better" — structure, reuse, a
KG-visible pattern the code contradicts — sourced from the diff and the
knowledge graph rather than from vibes. Grounding in the graph is what separates
it from the existing `critic`; `AdjudicateCandidates` already does this kind of
claim-checking.

**Rename it.** `capability.Advisor` already exists, means "hardware tier
advisor", and is wired into the router at `router.go:119`. A second `Advisor` in
the same binary is a permanent source of confusion. `RoleReviewer` is cheaper
than the collision.

## Is hierarchy needed at all?

**Yes, but only the authority half.** The scheduling half is already there and
works: the DAG is a wave scheduler with dependencies, and the supervisor pattern
it implements is the one that wins in production. What is missing is that a
supervisor cannot currently *restrict* what a worker may do — every agent has
every tool.

So: build tool scope per role, skip the org chart. A `RoleExecutive` that spawns
and directs other agents would duplicate what the planner and DAG executor
already do, and would reintroduce the goal-drift risk that made swarm not worth
building.

## Order, cheapest and highest-value first

1. **Wire `SubAgentConfig.Tools` through `Spawn`**, defaulting per role. Security
   fix, small, independent of everything else.
2. **Add `RoleReviewer`**, running after the acceptance gate, non-blocking,
   grounded in diff + KG.
3. **Make `result.Conflict` load-bearing**: on conflict with nothing checkable,
   one anchored round of `MsgCritiqueRequest` over the existing bus.
4. **Expose `/debate`** as an explicit one-shot verb over the same machinery.
5. **Seniority scalar** (tier, review-required, escalation target) if the role
   set grows enough to need it.

Steps 1 and 2 are worth doing whatever happens to debate. Step 3 is where the
bus and the `Conflict` flag stop being decoration.
