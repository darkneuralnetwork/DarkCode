# Contributing to DarkCode

## A note on this repository's history

The public git history begins in July 2026 with a small number of commits, and
it is not the real development timeline — the repository was re-initialised
before publication, collapsing the prior history into the initial commits.

We are saying so plainly because the alternative reads as concealment: anyone
evaluating this project will notice that a codebase of this size did not appear
in a handful of commits, and a silent gap invites a worse assumption than the
truth. Practically, it means **the commit history is not usable as evidence** of
development velocity, review process, or project age. Judge the code, the
tests, and the documentation instead. History from this point forward is real.

## Before you open a pull request

```bash
make ci       # gofmt check + go vet + build + race tests — exactly what CI runs
```

CI runs the same gate plus a cross-compile matrix (linux amd64/386, windows
amd64, darwin arm64), so a change that only builds on your machine is caught.

## What we care about

**Dependencies.** DarkCode has four direct third-party dependencies, all
terminal UI. That is a deliberate product property, not an accident — it is
what makes the binary auditable, statically linked, and viable in environments
that review their supply chain. A PR that adds a dependency needs to argue why
the standard library genuinely cannot do the job. "It is more convenient" is
not that argument.

**Honesty in output.** The agent must not report success it has not verified.
If a check did not run, say it did not run. See `orchestrator/acceptance.go`
for the pattern: unverifiable criteria are recorded as unverified rather than
counted as passing.

**Comments explain why.** The code says what it does. Comments should cover the
reasoning a future reader cannot recover — why a threshold has that value, what
failure a guard exists to prevent, what a heuristic's limits are.

**Tests describe behaviour.** Name the property under test, not the function.
Test what would actually break in production, including the failure paths.

## Areas we are especially interested in

- Language coverage in `intelligence/langparse.go` — extraction quality for
  TypeScript, Python, Rust and Java
- Knowledge-graph reasoning: better queries, better ranking, better use of
  confidence
- Cost reduction: anything that answers correctly with fewer paid tokens
- Benchmarks (`bench/`): more tasks, and honest reporting of results

## Security

Do not open a public issue for a vulnerability. See
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for scope and reporting.

## Licence

Contributions are accepted under GPL-3.0, the project's licence.
