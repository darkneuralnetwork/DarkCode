# DarkCode Architecture Audit

Status: **IN PROGRESS** (multi-week effort, started 2026-08-16). This document is
written incrementally as each phase completes. Every finding in this report must
trace to a specific file/line in the current codebase — no claim is carried over
from prior audits, notes, or documentation. This pass does not read `.md`/`.txt`
files in the repo for context (only Go source, `Makefile`, `go.mod`, `go.sum`,
config); it is based entirely on code and on commands actually run.

Git is untouched throughout this work at the user's instruction — no commits,
branches, or other git operations. (The working tree currently has no `.git`
directory at all.)

## 6. Baseline test/build results

Environment: `go version go1.26.6 linux/amd64`. All commands below were run
directly against the working tree exactly as checked out — no changes were made
before this baseline was captured.

| Command | Result | Notes |
|---|---|---|
| `go build -o <bin> .` | **PASS** [RAN] | Clean build, zero output, exit 0. |
| `go vet ./...` | **PASS** [RAN] | Zero findings. |
| `go test ./...` | **PASS** [RAN] | 46 packages `ok`, 3 packages report `[no test files]`: `github.com/darkcode` (root), `github.com/darkcode/ui`, `github.com/darkcode/cli/tui`. |
| `go test -race ./...` | **PASS** [RAN] | Same 46/3 split, zero races reported. |
| `make ci` (fmt-check → vet → arch-check → leak-check → build → test-race) | **PASS** [RAN] | All stages green. Notable: `arch-check.sh` enforces layering boundaries against `.arch-baseline` (8 metrics, all "at baseline", 0 regressions). `leak-check.sh --self-test` proves 12 leak-detection rules all fire correctly and 7 "clean" cases don't false-positive (secrets, commit attribution, vendor/model-name leaks). |

**Baseline verdict:** the codebase is not in a broken or bit-rotted state — build,
vet, full test suite, race detector, and the project's own CI gate (which
includes custom architecture-boundary and secret-leak self-tests) all pass
cleanly with no modifications. This is a materially different starting point
than "reproduce the bugs" implies; bugs found in this audit will be architectural/
design findings and edge cases surfaced by exercising the agent, not build breakage.

**Test-coverage gap surfaced immediately:** `ui` and `cli/tui` have zero test
files. Whether that matters depends on what's actually in them (Phase 2/3).

## 7. Reproduced bugs

(To be filled in as Phase 3/11 surface and reproduce concrete failures. Baseline
above shows no build/test-time failures to start from — anything here will come
from exercising the agent loop, tools, and edge cases directly.)

---

*Sections 1-5, 8-27 to follow as each phase completes. See task list for current phase.*
