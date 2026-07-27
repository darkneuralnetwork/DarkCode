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

### Dependencies we evaluated and declined

Measured, not assumed, so nobody has to redo the work. Baseline: a 15 MB
static binary, `CGO_ENABLED=0`, four direct dependencies.

| Candidate | What it would replace | Measured cost | Verdict |
| :-- | :-- | :-- | :-- |
| `go-tree-sitter` | `intelligence/langparse.go` (~280 lines) | **Does not compile with `CGO_ENABLED=0` at all** — the core `Node` type lives in a CGo file | **No.** Costs the static binary, the linux/386 + windows + darwin cross-compile matrix, and reproducible builds. Trading all of that for 280 lines is not close. |
| `modernc.org/sqlite` | knowledge-graph JSON persistence (~80 lines) | +4.3 MB binary, +25 modules | **No.** It would *add* code (schema, migrations, queries), not remove it. Worth revisiting only when a real graph outgrows the JSON store — for scale, never for tidiness. |
| `modelcontextprotocol/go-sdk` | `tools/mcp_client.go` + `server/mcp.go` | +3.9 MB, +11 modules including `cloud.google.com/go/compute/metadata`, `golang-jwt`, `oauth2` | **No.** Pulling GCP metadata access into an agent whose SSRF guard exists specifically to block cloud-metadata endpoints is a security-posture conflict, not just weight. |
| `go-github` | `tools/github.go` (~230 lines) | +2 modules for 8 endpoints of a very large API surface | **No.** Eight REST calls over stdlib `net/http` is less code than the integration layer would be. |

The general finding: the code that would benefit most from a library
(multi-language parsing) is the one case where the library is CGo-only, and
everything else we hand-rolled is domain logic — blast radius, defect
topology, semantic history — that no library provides. Adding dependencies
here would grow the binary and the audit surface without shrinking the source.

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
