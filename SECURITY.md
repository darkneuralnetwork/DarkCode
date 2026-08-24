# Security policy

## Reporting a vulnerability

Report privately through GitHub's [security advisory
form](https://github.com/darkneuralnetwork/DarkCode/security/advisories/new).
Do not open a public issue for anything exploitable.

Expect an acknowledgement within 72 hours. If a fix is warranted you will get a
release with the fix and credit in the advisory unless you ask otherwise.

## What is in scope

DarkCode runs a language model with tools on the developer's own machine. The
interesting failures are the ones where the model's output escapes the fence
the program put around it, so these carry the most weight:

- **Tool sandbox escape.** A tool call that writes outside the working
  directory, or runs a command the permission gate should have stopped
  (`permission/gate.go`, `permission/deny.go`).
- **Prompt injection reaching a privileged action.** Content fetched from a
  file, a URL or a tool result that causes a tool call the user never approved
  (`security/injection.go`).
- **SSRF.** Any path that reaches the network without going through
  `safeurl`, or that ignores air-gap mode (`safeurl/safeurl.go`).
- **Secret disclosure.** A provider key reaching a log, a memory store, a
  telemetry field, or the model's own context (`security/secrets.go`).
- **Anything reachable from the local web UI** (`server/`), which binds to
  loopback and is unauthenticated by design — a browser tab on the same machine
  is the trust boundary, so CSRF and DNS-rebinding paths count.

## What is out of scope

- Findings that require the attacker to already have code execution as the user
  running DarkCode. The agent is a program the user runs deliberately; it is
  not a privilege boundary against its own operator.
- The model doing something unhelpful, wrong, or expensive. That is a quality
  bug — open an issue.
- Vulnerabilities in a model provider's API rather than in this client.
- Reports produced by a scanner with no reachability analysis and no proof of
  exploitability. `govulncheck` runs in CI precisely because "this module is in
  go.sum and has a CVE" is not yet a finding.

## Supported versions

The latest tagged release. This project ships a single static binary; there is
no backport branch.

## How releases can be verified

Every release is built by `.github/workflows/release.yml` and carries:

- `SHA256SUMS` for each artifact.
- An SBOM read back out of the linked binary rather than generated beside it.
- A build-provenance attestation recording the commit, the workflow and the
  runner. Verify with `gh attestation verify <artifact> --repo darkneuralnetwork/DarkCode`.
- A reproducibility check: the release job builds twice and fails if the bytes
  differ, so anyone can rebuild the tag and compare.

A signature says someone vouched for a file. The attestation says where the
file came from — that is the one that matters for a supply-chain question.

## What runs on every change

| Check | Catches |
|---|---|
| `scripts/leak-check.sh` | this project's rules: vendor names in branches and filenames, AI attribution in commit messages, files outside the path allowlist, secret-shaped diffs |
| gitleaks | secrets, over full history, with the maintained provider ruleset |
| `govulncheck` | vulnerable dependencies **that our code actually calls** |
| CodeQL (`security-extended`) | injection and taint paths |
| `make arch-check` | layering regressions, which is how a call escapes its guard in the first place |
| race detector | data races, on every test run |

Secret scanning and push protection are enabled on the repository, and
Dependabot opens pull requests for both Go modules and Actions.
