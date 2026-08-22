# Recon: Model Router / Provider / Capability layer (Tier 1)

> **Note:** this is a point-in-time subsystem pass, one of five inputs consolidated into `../../DARKCODE_RECON_REPORT.md`. Several items marked UNKNOWN or unverified here (notably: the SSRF/air-gap dial-time logic in `safeurl/safeurl.go`, `tools/terminal.go`'s `Sandbox.MustRefuse()` call, prompt-cache mechanics, and the GPU `-ngl` layer-count calculation) were resolved during consolidation. **Where this file and the consolidated report disagree, the consolidated report is authoritative.**


Scope: `llm/` (14), `router/` (16), `provider/` (7 + `provider/embedded/`), `modelport/` (2),
`capability/` (7), `compression/` (6). FACT = cites a file; INFERENCE = interpretation;
UNKNOWN = unresolved.

## Purpose
Model selection (routing mode + tier + task-type offload), a single generic LLM HTTP client
reused across every cloud provider, the embedded local llama.cpp lifecycle, hardware-capability
detection driving local-model decisions, and STM context compression.

## Entry Points
- `router.NewRouter(mode, emitter) *Router` — `app_wireup.go:322`.
- `(*Router).Route(tier, complexity, taskDesc) (core.LLMClient, string, error)` —
  `router/router.go:316`. The main per-call model-selection entry point.
- `llm.NewClient(baseURL, apiKey, model) *Client` — `llm/client.go`.
- `embedded.NewProviderWithDirs`, `PlanLocalLoad` — `provider/embedded/`.

## Public API
`Router.Route`, `PlannerRoute`, `SetMode`/`GetMode`, `SetForceLocal`, `SetEnableLocalOffloading`,
`SetAdvisor`, `SetModelRole`, `MarkPrimary`, `ModelCount`, `HasModel`, `SetReliabilityPath`.
`llm.Client.ChatCompletion`/`ChatCompletionStream`/`CreateEmbedding`/`Ping`. `llm.WithRetry`,
`llm.WrapCloud`, `llm.NewKeyPool`. `capability.Detect(ctx)`, `capability.NewAdvisor(caps)`.
`embedded.PlanLocalLoad(caps, candidates, loraBytes, desiredCtx) LoadPlan`.

## Internal Components — the central architectural finding
**DarkCode is architecturally "one generic OpenAI-chat-completions-shaped HTTP client + N
provider configs," not N native per-vendor SDKs.** Verified directly:
- `llm/client.go:293,401` — both `ChatCompletion` and `ChatCompletionStream` POST to
  `c.endpointURL("/chat/completions")` **unconditionally, regardless of provider**.
- `config/providers.go` catalogues 10+ providers, and **every one's `BaseURL` is an
  OpenAI-compatible endpoint**: `google` → `https://generativelanguage.googleapis.com/v1beta/
  openai` (`providers.go:104` — Google's own OpenAI-compat shim), `groq` →
  `https://api.groq.com/openai/v1`, `deepseek` → `https://api.deepseek.com`, `mistral` →
  `https://api.mistral.ai/v1`, `xai` → `https://api.x.ai/v1`, `together` →
  `https://api.together.xyz/v1`, `openrouter` → `https://openrouter.ai/api/v1` (itself a
  multiplexer, listing `anthropic/claude-3.5-sonnet` etc. as pass-through model IDs).
- **Anthropic is the interesting case**: `config/providers.go:76` sets `BaseURL:
  "https://api.anthropic.com/v1"`, and the client still POSTs to `/chat/completions` off that
  base — i.e. it hits Anthropic's own OpenAI-compatibility endpoint
  (`api.anthropic.com/v1/chat/completions`, a real Anthropic-provided shim), NOT Anthropic's
  native `/v1/messages` Messages API. `llm/client.go:140-142` and `:351-353` layer
  Anthropic-specific auth on top (`x-api-key` instead of `Bearer`, plus a mandatory
  `anthropic-version: 2023-06-01` header) — confirming the generic client still needs per-provider
  patches even when using the compat endpoint. **This is a real, specific architectural fact**:
  the README's "Supported providers: OpenAI, Anthropic, ..." is accurate in terms of *reachability*
  but the mechanism is "one client speaking the OpenAI wire format to each provider's
  OpenAI-compatible surface," not "N native integrations" — meaning any Anthropic-only feature not
  exposed through their OpenAI-compat shim (e.g. certain extended-thinking response shapes) may be
  unavailable or need special-casing. `llm/thought_signature_test.go` existing suggests at least
  some Claude-specific response handling (thinking/signature blocks) has been special-cased, so
  this is not entirely naive — but the core transport is uniform.
- `provider/openai_provider.go`, `ollama_provider.go`, `lmstudio_provider.go` — thin
  provider-specific listing/discovery adapters (e.g. `ListModels` against each one's native
  model-listing endpoint), separate from the chat-completion path above, which stays generic.
- Google auth is also special-cased: `llm/client.go:161-163` sets `x-goog-api-key` because
  "Google Gemini API gateway often rejects Bearer for API keys."
- **Key pool / credential rotation** (`llm/keypool.go`): doc comment confirms the real design —
  "Free and low-tier API keys are rate-limited per key, not per user, so a single key turns every
  burst into a 429 storm. A pool spreads calls across [keys]... after a rate-limit response" —
  did not read the rotation/parking algorithm body itself, but the mechanism's purpose and trigger
  (429) are confirmed from the header comment; `llm/ratelimit.go` is a separate, dedicated file
  for rate-limit handling.
- **Retry/backoff** (`llm/retry.go`) — comment at `retry.go:86` references classifying failures
  across "openai/anthropic-shim/gemini... and the embedded llama-server," confirming retry
  classification is provider-aware even though the transport is generic.
- **Reasoning effort / prompt caching**: `llm/window.go:44` has an "--- Anthropic ---" section
  (context-window table, presumably), and `llm/cache.go:16-29` explicitly documents
  provider-specific cache semantics: `explicitCacheProviders = map[string]bool{"anthropic": true,
  "openrouter": true}` (`cache.go:24`) — i.e. **prompt caching is real and provider-gated**, not a
  blanket claim; a comment notes a cache breakpoint added for Anthropic "must not follow it to a"
  [different provider on failover] — confirms the code is aware caching syntax is
  provider-specific and guards against leaking one provider's cache-control markup into a request
  to a different one.
- **Local resource governor** (`provider/embedded/governor.go`, `PlanLocalLoad`,
  `governor.go:115-155`): real, non-trivial algorithm — sorts candidate models largest-first,
  for each tries context sizes from `desiredCtx` (or 32768 default) down to a floor, halving each
  step, computing `total = modelBytes + kvBytesPerCtxToken(model)*ctx + loraBytes + overhead` and
  accepting the first (largest model, largest context) combination that fits the budget; clamps
  requested context to the model's trained/native window (`catalogContextWindow`) since
  "llama-server rejects -c beyond the trained window." Reduces parallelism (`NParallel`) to 1
  under tight RAM (`ctx < 16384`) to preserve a usable effective window. **Confirmed by a real,
  meaningful test** (`governor_test.go:25-42`, `TestPlanLocalLoad_11GBNeverPlansOverBudget` —
  explicitly asserts the planned total never exceeds budget "this is the swap-thrash hang" and
  that launch values are non-zero). **This is genuine, well-designed capacity-planning logic, not
  a superficial stub.**
- **Compression** (`compression/`): `budget.go`, `importance.go` (message-importance scoring for
  what survives compaction), `compressor.go` — orchestrated by `orchestrator/kernel_execute.go`'s
  token-window-based trigger (already documented in the orchestrator recon file).

## Dependencies
`router` imports `capability`, `compression`, `core`, `ui`. `llm` imports `config`, `core`,
`internal/strutil`, `metrics`, `safeurl`. `provider` imports `config`, `core`, `llm`, `safeurl`.
`provider/embedded` imports `capability`, `core`, `llm`, `observability`, `provider`, `safeurl`.

## Dependents
`router`: fan-in 7 (`orchestrator`, `verb`, `cli`, `server`, `agents`, `loop`, root). `llm`: fan-in
8. `provider`/`provider/embedded`: consumed by root `main` and `cli`/`server` (client-factory
construction happens in `app_wireup.go`, not inside orchestrator — orchestrator only sees
`core.LLMClient` and `router`).

## Data
`model_reliability.json` (path set via `SetReliabilityPath`, `app_wireup.go:691`) — per-model
accumulated reliability, read by `router/role_tracker.go` (file exists, not opened in depth,
name strongly implies this is real per-role success/failure tracking feeding future routing
decisions, consistent with the README's "stops asking a model that keeps failing a role" claim —
**PARTIALLY VERIFIED**: file exists and is wired with a persistence path; exact algorithm not
read).

## Control Flow (representative: routing one model call)
1. Caller (orchestrator) calls `router.Route(tier, complexity, taskDesc)`.
2. `router.Classify(taskDesc)` (`router/classifier.go`) determines task type; if local-offload is
   enabled and the task is tiny/medium-local-appropriate, that overrides tier selection outright
   (`router.go:326-333`) — a cost-saving intercept that runs *before* the routing-mode switch.
3. Otherwise, dispatch on mode: **single** → best available for the requested tier; **escalation**
   → fast tier below a complexity threshold, reasoning tier at/above it (`router.go:341-348`);
   **consensus** → primary is always reasoning tier, actual multi-model fan-out happens one layer
   up in `orchestrator/consensus.go`, not inside `Route` itself.
4. `selectBestAvailable(tier)` resolves an actual registered client; if none and `forceLocal` is
   set, returns a hard, explicit error rather than silently falling back to cloud
   (`router.go:365-372`) — **confirms the "force-local hard guarantee" claim already noted by the
   orchestrator/wireup investigation, now verified at the actual selection point**.
5. Client's `ChatCompletion`/`ChatCompletionStream` builds a `/chat/completions`-shaped request,
   applies provider-specific auth headers, sends via the retry/key-pool wrapper.

## External Effects
Outbound HTTPS to whichever provider's OpenAI-compatible endpoint; local HTTP to `llama-server`
for embedded models; writes `model_reliability.json` and (elsewhere) the cascade log.

## Business Rules
- **Force-local hard guarantee** — VERIFIED at the actual routing decision point (`router.go:
  365-372`), not just at startup wiring.
- **Local-offload task-type intercept runs before routing-mode logic** — a genuinely deliberate
  cost-saving design: certain task types always go local regardless of single/escalation/consensus
  mode, unless local offloading is disabled.
- **Resource governor never plans over budget** — verified by both the algorithm's structure and
  a real test asserting the exact failure mode it exists to prevent (swap-thrashing).
- **Cache-control markup is provider-scoped and doesn't leak across a provider switch** — a subtle,
  real correctness concern that's explicitly guarded (`llm/cache.go`).
- **`unbounded-completions 0`** (`.arch-baseline`): did not independently confirm every
  `ChatCompletion` call site sets `MaxTokens > 0`; took the arch-check ceiling at face value per
  the same reasoning as the interfaces recon file (the check is a real, running CI gate).

## Concurrency
`Router` state (`clients`, `models`, `mode`, `forceLocal`, `enableLocalOffloading`) is guarded by
`r.mu sync.RWMutex` — read-heavy access pattern (routing decisions) uses `RLock`, writes
(`SetMode`, model registration) use `Lock`. `KeyPool` presumably needs its own concurrency-safe
rotation state under concurrent requests — not read in depth.

## Error Handling
- `ErrNoBaseURL` is deliberately a plain (non-`net.Error`) error so the retry layer classifies it
  as **permanent** rather than retryable — "retrying a request with no URL only produces the same
  ... failure five times" (`llm/client.go:166-172`) — a small but genuinely thoughtful detail.
- `checkEndpoint()` runs at the top of every request method for a fast, clear failure instead of
  an opaque URL-scheme error from deep inside `net/http`.
- Force-local with no local model: explicit, actionable error (not silent fallback).

## Tests
`governor_test.go` (real, meaningful assertions, see above), `router_test.go`, `classifier_test.go`,
`distribute_test.go`, `escalate_test.go`, `force_local_test.go`, `intent_test.go`,
`role_tracker_test.go`, `cache_test.go`, `context_overflow_test.go`, `model_default_test.go`,
`ratelimit_test.go`, `retry_classify_test.go`, `thought_signature_test.go`, `window_test.go`,
`fetch_test.go`, `tier_test.go`, `budget_fit_test.go`, `fit_toolpair_test.go`, `importance_test.go`.
Broad, function-name-matched coverage across nearly every component described above.

## Important Files
| File | Purpose |
|---|---|
| `router/router.go` | Core `Route`/`PlannerRoute` model-selection logic |
| `router/classifier.go` | Task-type classification for local-offload intercept |
| `router/escalate.go` | Escalation-mode complexity threshold logic |
| `router/role_tracker.go` | Per-model/role reliability tracking |
| `llm/client.go` | Generic OpenAI-shaped HTTP client, provider auth patching |
| `llm/keypool.go` | Multi-key rotation / rate-limit parking |
| `llm/retry.go` | Provider-aware retry/backoff classification |
| `llm/cache.go` | Provider-scoped prompt-cache-control handling |
| `llm/window.go` | Per-model context-window table |
| `provider/provider.go` | `ClientOptions`/provider-kind abstraction |
| `provider/fetch.go` | Provider-specific model-listing HTTP calls |
| `provider/embedded/governor.go` | Local-model resource-fit planner |
| `provider/embedded/client.go` / `embedded_stub.go` | llama-server subprocess lifecycle |
| `capability/detector_linux.go` (+darwin/windows) | Real hardware-capability probing |
| `capability/advisor.go` | Hardware-tier → local-model-eligibility decision |
| `compression/compressor.go` | STM compaction implementation |
| `compression/importance.go` | Message-importance scoring for what survives compaction |
| `config/providers.go` | The static provider/model catalogue (base URLs, pricing, context) |

## Unknowns
- Exact key-rotation/parking algorithm body in `llm/keypool.go` (purpose confirmed via doc
  comment, mechanics not traced).
- Exact per-OS implementation depth in `capability/detector_{linux,darwin,windows}.go` — real
  syscalls vs. heuristics not independently confirmed for all three platforms (only Linux is
  relevant to the current environment; not opened this pass).
- `router/role_tracker.go`'s actual scoring/decay algorithm.
- Whether `/chat/completions` is really universal across every listed provider or whether any
  (e.g. `azure`, seen in the grep output but not detailed here) needs a different path suffix —
  Azure OpenAI typically uses a deployment-specific path shape; not confirmed whether this is
  handled as a special case or is a latent gap.

## Claim verification table
| Claim | Verdict | Evidence |
|---|---|---|
| "Supported providers: OpenAI, Anthropic, OpenRouter, Google, Groq, DeepSeek, Mistral, xAI, Together, Ollama, LM Studio, embedded" | VERIFIED (reachability), but mechanism is materially different from "N native SDKs" | All confirmed present in `config/providers.go` with real base URLs; transport is one generic OpenAI-shaped client hitting each provider's OpenAI-compat surface, not N per-vendor request/response translators |
| Credential rotation across multiple keys, parking rate-limited ones | LIKELY VERIFIED (purpose confirmed, mechanics not traced) | `llm/keypool.go` doc comment describes exactly this; algorithm body not read |
| Prompt caching | VERIFIED, and correctly provider-scoped | `llm/cache.go:16-29`, explicit provider allowlist + cross-provider leak guard |
| Local resource governor (weights+KV+LoRA+overhead vs free memory, refuses swap-thrash) | VERIFIED, real and well-tested | `provider/embedded/governor.go:115-155` + `governor_test.go:25-42` |
| Per-model reliability stops routing to models that keep failing a role | PARTIALLY VERIFIED | Persistence path and file wired; scoring algorithm not read |
