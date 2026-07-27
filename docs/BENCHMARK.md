---
title: Running the benchmark
---

# Running the benchmark

The harness is complete and the agent has been driven end-to-end through it.
What is missing is a score, and the only thing between here and one is model
quota.

## One command

```sh
make build
make bench          # writes bench-report.json
```

`make bench` runs every task in `bench/tasks/` in its own temporary workspace:
`setup.sh` builds the starting state, the agent runs one-shot via `-q`, and
`verify.sh`'s **exit status alone** decides pass or fail. Nothing reads the
agent's prose. That is deliberate — a benchmark an agent can talk its way
through measures nothing.

## Configuring a model

The agent reads `~/.darkcode/config.json`. For Gemini:

```json
{
  "provider": "gemini",
  "model": "gemini-2.5-flash",
  "base_url": "https://generativelanguage.googleapis.com/v1beta/openai",
  "api_key": "…",
  "safety_level": "off",
  "sandbox": "off",
  "agentic_loop": true,
  "max_loops": 8
}
```

`safety_level: off` and `sandbox: off` matter: each task runs in a throwaway
directory, and an approval prompt in a headless run blocks forever.

`base_url` is required. Without it the client falls back to `127.0.0.1:0` and
every task fails with a connection refused that looks like a network problem
rather than a missing setting.

## Quota

Free-tier Gemini keys carry a small daily request cap — around 20 requests per
day. One benchmark task costs several requests, so the four shipped tasks can
exhaust a free key in a single run, and the quota error arrives as a task
*failure*, which scores worse than not running at all.

Check the report before believing a number:

```sh
jq '.results[] | select(.error | test("429|quota"))' bench-report.json
```

Any hit there means the run is invalid, not that the agent is bad. Use a key
with billing enabled, or a local model via Ollama, for a score worth
publishing.

## What a real run needs

The report this work responds to asks for 100 tasks (TBLite). Four ship here.
Authoring the rest is content work, not engineering: each task is a directory
with `task.json`, `setup.sh` and `verify.sh`, and CI already checks that every
shipped fixture is solvable.
