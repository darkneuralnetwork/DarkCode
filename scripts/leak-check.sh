#!/usr/bin/env bash
# leak-check.sh — refuse to let a leak reach the remote.
#
# WHY THIS EXISTS
#
# A prior session left eight untracked drafts beside the source, two with a
# vendor name in the filename, and .gitignore covered only one of the three
# sensitive patterns. One `git add -A` would have published all of it, and
# nothing — no hook, no CI step — would have stopped it. This is that stop.
#
# ONE SCRIPT, TWO LAYERS
#
# The git hook (scripts/install-hooks.sh wires it to pre-commit + pre-push) is
# convenience: it fails fast on the developer's machine. This same script run
# in CI (.github/workflows/ci.yml, job `leak-guard`) is the guarantee: a commit
# made with --no-verify, or on a machine with no hook installed, still cannot
# merge. Both call the SAME rules here — every check function runs in every
# mode. They differ only in SCOPE: the hooks see the staged set or the push
# range, CI additionally sweeps every tracked file. That asymmetry is
# deliberate; a rule running in one mode and not the other is not, and was the
# defect that let four new top-level packages pass pre-push and fail CI.
#
# WHAT IT DOES NOT DO
#
# It does not scan source code for vendor words. The provider catalogue, the
# pricing tables, API endpoints, env-var names (ANTHROPIC_API_KEY) and the
# protocol-compat comments ("Gemini folds system messages...") all name vendors
# legitimately — 351 lines across 40 files. A content scan there is all false
# positive. The leak vectors that are real and tractable are filenames, branch
# names, commit attribution, secret-shaped strings, and files landing outside
# the expected tree. Those are the rules below.
#
# THE MODEL-ID EXCEPTION
#
# `claude-sonnet-5` is a legitimate model identifier
# (surfaces/cli/completer_test.go has it). The vendor-name rules skip any
# token in `vendor-word-<alnum>-<digit>`
# form, so a model id never trips the guard while a bare "Claude" attribution
# does.
#
# Usage:
#   leak-check.sh --staged            # pre-commit: staged files + branch
#   leak-check.sh --commit-msg FILE   # commit-msg hook: the message
#   leak-check.sh --push              # pre-push: range being pushed + branch
#   leak-check.sh --ci [BASE]         # CI: diff vs BASE (default origin/main), tree, branch
#   leak-check.sh --self-test         # prove every rule fires; run in CI

set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

# This file necessarily contains the very patterns it hunts for — the SECRET
# regex and the self-test's fake keys (sk-…, AKIA…, a PRIVATE KEY header). The
# content scan excludes it, or the guard would refuse to let itself be
# committed. Nothing else is exempt.
SELF_EXCLUDE=':(exclude)scripts/leak-check.sh'

# ---------------------------------------------------------------- patterns ---

# Vendor names this project keeps out of branch names and filenames.
VENDOR='claude|anthropic|openai|chatgpt|copilot|gemini|bard|hermes'

# Provider/protocol integration files legitimately carry a vendor name: the
# whole point of model/provider/openai_provider.go and
# surfaces/server/openai_compat.go is to speak that vendor's API.
# "OpenAI-compatible" is a de-facto wire standard, not attribution. A draft
# named after a vendor does not match this shape.
INTEGRATION='(_provider|_compat)(_test)?\.go$'

# A model identifier: vendor-ish word then a hyphenated tail ending in a digit
# (claude-sonnet-5, gemini-2.5-flash, gpt-4o, o1-mini). Interior hyphens are in
# the class so multi-part ids match whole — without that, "claude-sonnet-5"
# stops at the second hyphen and the vendor rule flags a legitimate model id
# (the self-test caught exactly this). Allowed anywhere.
MODELID='(claude|gemini|gpt|o[0-9]|llama|mistral|qwen|deepseek|gemma|phi)-[a-z0-9.-]*[0-9]'

# AI attribution in a commit message — the specific thing this project bans.
# Not a bare vendor word: "raise the anthropic rate limit" is a fine commit.
ATTRIBUTION='co-authored-by:.*(claude|anthropic|openai|gpt|copilot|bard|gemini)|generated with .*(claude|chatgpt|copilot|codex)|🤖|as an ai (language )?model|with claude code'

# Sensitive draft artifacts — the drafts from the incident, defence in depth
# behind .gitignore (a `git add -f` bypasses the ignore).
SENSITIVE='(^|/)(THESIS|HANDOFF_PROMPT|NEXT_SESSION_PROMPT|DARKCODE_VS_[A-Z_]+|project_detail|detail_[a-z0-9_]+|[a-z0-9_]+_prompt|multi_debate)\.(md|txt)$|(^|/)mywebssssite/'

# Secret-shaped strings.
SECRET='sk-[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|xox[baprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{35}|-----BEGIN [A-Z ]*PRIVATE KEY-----'
# Secret-shaped filenames.
SECRET_FILE='(^|/)\.env(\.|$)|(^|/)id_(rsa|dsa|ecdsa|ed25519)$|\.(pem|p12|pfx|keystore)$'

# Top-level paths a tracked file is allowed to live under. A new file outside
# this set is either misplaced or a stray draft. Keep in step with the tree.
ALLOWED_TOP='bench|build|cmd|docs|infra|internal|kernel|memory|model|scripts|surfaces|tools|\.github|\.githooks'
# Top-level files that are allowed to exist (not under a directory).
ALLOWED_FILE='build\.sh|Makefile|Dockerfile|\.dockerignore|go\.mod|go\.sum|README\.md|CONTRIBUTING\.md|SECURITY\.md|LICENSE|SIGNING-KEY\.asc|\.gitignore|\.gitleaks\.toml|\.arch-baseline'

# ------------------------------------------------------------ check funcs ---
# Each prints one line per violation to stdout and nothing when clean, so the
# self-test can feed synthetic input and count what fires.

# strip_modelids removes model-id tokens so a vendor rule does not trip on them.
strip_modelids() { sed -E "s/$MODELID//gI"; }

check_branch() { # $1 = branch name
  printf '%s\n' "$1" | strip_modelids | grep -iqE "$VENDOR" \
    && echo "branch: '$1' contains a vendor name — it must never reach the remote"
  return 0
}

check_commit_msg() { # $1 = message text
  if printf '%s' "$1" | grep -iqE "$ATTRIBUTION"; then
    echo "commit message: contains AI attribution (Co-Authored-By / Generated-with / 🤖)"
  fi
  return 0
}

check_filenames() { # stdin = NUL-free newline list of paths
  local f
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if ! printf '%s\n' "$f" | grep -iqE "$INTEGRATION" \
       && printf '%s\n' "$f" | strip_modelids | grep -iqE "$VENDOR"; then
      echo "file: '$f' has a vendor name in its path"
    fi
    if printf '%s\n' "$f" | grep -iqE "$SENSITIVE"; then
      echo "file: '$f' is a sensitive draft artifact"
    fi
    if printf '%s\n' "$f" | grep -iqE "$SECRET_FILE"; then
      echo "file: '$f' looks like a secret/key file"
    fi
  done
  return 0
}

check_paths() { # stdin = newline list of paths; flags any outside the allowlist
  local f top
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if [[ "$f" == */* ]]; then
      top="${f%%/*}"
      printf '%s\n' "$top" | grep -qE "^($ALLOWED_TOP)$" \
        || echo "path: '$f' is under unexpected top-level '$top' (not in the allowlist)"
    else
      printf '%s\n' "$f" | grep -qE "^($ALLOWED_FILE)$" \
        || echo "path: '$f' is an unexpected top-level file (not in the allowlist)"
    fi
  done
  return 0
}

check_secrets_content() { # stdin = text (a diff or file contents)
  grep -inE "$SECRET" | sed -E 's/^/secret: /' | head -20
  return 0
}

# ---------------------------------------------------------------- drivers ---

fail_if() { # $1 = collected violations text
  if [ -n "$1" ]; then
    echo "leak-check: refusing — a leak would reach the remote:" >&2
    printf '%s\n' "$1" | sed 's/^/  ✗ /' >&2
    echo >&2
    echo "Fix the above, or if a match is a false positive, narrow the rule in scripts/leak-check.sh." >&2
    exit 1
  fi
  echo "leak-check: clean"
}

current_branch() { git rev-parse --abbrev-ref HEAD 2>/dev/null; }

run_staged() {
  local v="" files
  files="$(git diff --cached --name-only --diff-filter=AM)"
  v+="$(check_branch "$(current_branch)")"$'\n'
  v+="$(printf '%s\n' "$files" | check_filenames)"$'\n'
  v+="$(printf '%s\n' "$files" | check_paths)"$'\n'
  v+="$(git diff --cached --diff-filter=AM -- . "$SELF_EXCLUDE" | grep -E '^\+' | check_secrets_content)"$'\n'
  fail_if "$(printf '%s' "$v" | grep -c . >/dev/null; printf '%s' "$v" | grep .)"
}

run_commit_msg() {
  local v
  v="$(check_commit_msg "$(cat "$1")")"
  fail_if "$(printf '%s' "$v" | grep .)"
}

run_push() {
  local v="" range base files
  base="$(git merge-base HEAD origin/main 2>/dev/null || git rev-parse HEAD~1 2>/dev/null)"
  range="${base:+$base..HEAD}"
  v+="$(check_branch "$(current_branch)")"$'\n'
  if [ -n "$range" ]; then
    files="$(git diff --name-only --diff-filter=AM "$range")"
    v+="$(printf '%s\n' "$files" | check_filenames)"$'\n'
    # The path allowlist runs here too. It used to run only in --ci, so a new
    # top-level package passed pre-push and then failed the leak-guard job —
    # four of them did. A hook that says clean where CI says refuse is worse
    # than no hook.
    v+="$(printf '%s\n' "$files" | check_paths)"$'\n'
    v+="$(git log --format='%B' "$range" | check_commit_msg /dev/stdin 2>/dev/null || true)"$'\n'
    v+="$(git diff "$range" -- . "$SELF_EXCLUDE" | grep -E '^\+' | check_secrets_content)"$'\n'
    # attribution over each message in the range
    while IFS= read -r msg; do v+="$(check_commit_msg "$msg")"$'\n'; done < <(git log --format='%B%x00' "$range" | tr '\0' '\n')
  fi
  fail_if "$(printf '%s' "$v" | grep .)"
}

run_ci() {
  local base="${1:-origin/main}" v="" files
  git rev-parse --verify -q "$base" >/dev/null 2>&1 || base="$(git rev-parse HEAD~1 2>/dev/null)"
  v+="$(check_branch "$(current_branch)")"$'\n'
  # Files added/modified vs base, plus every tracked file for the path allowlist.
  files="$(git diff --name-only --diff-filter=AM "$base"...HEAD 2>/dev/null)"
  v+="$(printf '%s\n' "$files" | check_filenames)"$'\n'
  v+="$(git ls-files | check_filenames)"$'\n'
  v+="$(git ls-files | check_paths)"$'\n'
  v+="$(git diff "$base"...HEAD -- . "$SELF_EXCLUDE" 2>/dev/null | grep -E '^\+' | check_secrets_content)"$'\n'
  while IFS= read -r msg; do v+="$(check_commit_msg "$msg")"$'\n'; done < <(git log --format='%B%x00' "$base"..HEAD 2>/dev/null | tr '\0' '\n')
  fail_if "$(printf '%s' "$v" | grep .)"
}

# self_test proves each rule fires on a crafted violation and passes clean
# input — because a rule that silently never matches is exactly the defect
# arch-check found in itself (a boundary computed but never evaluated).
run_self_test() {
  local fails=0
  assert_fires() { # $1 = rule label, $2 = violation count (must be >0)
    if [ "$2" -gt 0 ]; then echo "ok    $1 fires ($2)"; else echo "DEAD  $1 did not fire — the rule is not running"; fails=1; fi
  }
  assert_clean() { # $1 = label, $2 = count (must be 0)
    if [ "$2" -eq 0 ]; then echo "ok    $1 clean"; else echo "FALSE $1 flagged a legitimate input ($2)"; fails=1; fi
  }

  assert_fires "branch-name"        "$(check_branch 'claude/some-feature' | grep -c .)"
  assert_fires "commit-attribution" "$(check_commit_msg 'fix bug

Co-Authored-By: Claude <noreply@anthropic.com>' | grep -c .)"
  assert_fires "commit-robot-emoji" "$(check_commit_msg '🤖 Generated with Claude Code' | grep -c .)"
  assert_fires "vendor-filename"    "$(printf 'docs/claude_prompt.md\n' | check_filenames | grep -c .)"
  assert_fires "sensitive-filename" "$(printf 'THESIS.md\n' | check_filenames | grep -c .)"
  assert_fires "secret-filename"    "$(printf 'deploy/id_rsa\n' | check_filenames | grep -c .)"
  assert_fires "path-allowlist"     "$(printf 'random_top/thing.md\n' | check_paths | grep -c .)"
  assert_fires "secret-openai"      "$(printf 'key := "sk-abcdef0123456789ABCDEFGHIJ"\n' | check_secrets_content | grep -c .)"
  assert_fires "secret-aws"         "$(printf 'AKIAIOSFODNN7EXAMPLE\n' | check_secrets_content | grep -c .)"
  assert_fires "secret-privkey"     "$(printf -- '-----BEGIN RSA PRIVATE KEY-----\n' | check_secrets_content | grep -c .)"

  # Clean inputs — especially the model-id exception the vendor rules must honour.
  # A vendor draft under provider/ must still be caught — the exception is for
  # integration files by shape, not for the directory.
  assert_fires "vendor-draft-in-provider" "$(printf 'model/provider/claude_notes.md\n' | check_filenames | grep -c .)"

  assert_clean "modelid-branch"     "$(check_branch 'feature/tune-gpt-4o' | grep -c .)"
  assert_clean "modelid-filename"   "$(printf 'infra/config/claude-sonnet-5.json\n' | check_filenames | grep -c .)"
  assert_clean "integration-openai" "$(printf 'model/provider/openai_provider.go\nsurfaces/server/openai_compat.go\n' | check_filenames | grep -c .)"
  assert_clean "provider-commit"    "$(check_commit_msg 'raise the anthropic provider rate limit' | grep -c .)"
  assert_clean "normal-file"        "$(printf 'memory/memory/retrieval.go\n' | check_filenames | grep -c .)"
  assert_clean "normal-path"        "$(printf 'kernel/orchestrator/kernel.go\n' | check_paths | grep -c .)"
  assert_clean "modelid-content"    "$(printf '  Model: "claude-sonnet-5",\n' | check_secrets_content | grep -c .)"

  echo
  if [ "$fails" -ne 0 ]; then echo "leak-check self-test FAILED"; exit 1; fi
  echo "leak-check self-test passed — every rule fires and the model-id exception holds"
}

case "${1:---ci}" in
  --staged)     run_staged ;;
  --commit-msg) run_commit_msg "${2:?commit message file required}" ;;
  --push)       run_push ;;
  --ci)         run_ci "${2:-}" ;;
  --self-test)  run_self_test ;;
  *) echo "unknown mode: $1" >&2; exit 2 ;;
esac
