#!/usr/bin/env bash
# arch-check.sh — enforce the architectural layering boundaries.
#
# WHY THIS EXISTS
#
# Every layering rule in this codebase has been held by convention. httpx's own
# comment claimed "a CI grep fails the build on http.DefaultClient outside this
# package"; no such step existed. THESIS.md recommended adding these greps
# (Phase 3.3) and it was never done. Meanwhile 23 LLM call sites and 34 memory
# write sites accumulated outside their owning layers, because nothing counted.
#
# HOW IT WORKS
#
# Each boundary has a baseline count in .arch-baseline. The check fails when a
# count goes UP — a new violation cannot land silently. When a count goes DOWN
# it prints the new number and tells you to ratchet the baseline, so migration
# progress is locked in and cannot regress.
#
# This mirrors the coverage-floor convention: floors only rise, ceilings only
# fall. A boundary at 0 is a boundary that is genuinely enforced.
#
# Usage:  scripts/arch-check.sh          # check against .arch-baseline
#         scripts/arch-check.sh --list   # show the offending sites per boundary
#         scripts/arch-check.sh --update # rewrite .arch-baseline to current counts

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

BASELINE_FILE=".arch-baseline"
MODE="check"
case "${1:-}" in
  --list)   MODE="list" ;;
  --update) MODE="update" ;;
  "")       ;;
  *) echo "unknown flag: $1" >&2; exit 2 ;;
esac

# scan PATTERN EXCLUDE_DIRS_REGEX
#
# Greps non-test Go files for PATTERN, skipping packages matched by
# EXCLUDE_DIRS_REGEX, and strips comment lines. --exclude-dir='.claude' keeps
# nested agent git worktrees (.claude/worktrees/*) out of every count: those are
# full checkouts of other branches, so scanning them multiplied each boundary by
# the number of worktrees present and failed the check with zero code change.
# The comment strip matters: a
# comment mentioning http.DefaultClient is documentation, not a construction,
# and counting it produces a false violation (this exact false positive was hit
# while writing these baselines).
scan() {
  local pattern="$1" exclude="$2"
  grep -rn --include='*.go' --exclude-dir='.claude' -E "$pattern" . 2>/dev/null \
    | grep -v '_test\.go:' \
    | { [ -n "$exclude" ] && grep -vE "$exclude" || cat; } \
    | awk -F: '{ line=$0; sub(/^[^:]*:[0-9]+:/, "", line);
                 sub(/^[ \t]+/, "", line);
                 if (line !~ /^\/\// && line !~ /^\*/ && line !~ /^\/\*/) print }'
}

# Audit().RecordAction and Learning().RecordFeedback are deliberately NOT in
# the memory-writes pattern. They are logs ABOUT the agent — which tool ran at
# what risk, whether a strategy worked — not facts about the world, and each
# has exactly one possible destination. The gateway exists to decide placement
# among five stores; a record with one destination has no placement to decide,
# and routing it through a fact manager would only obscure that it is telemetry.
#
# Boundary definitions. Keep NAME, PATTERN and EXCLUDE aligned by index.
NAMES=(
  llm-calls
  memory-writes
  kernel-entry
  orchestrator-impl-imports
  raw-http-clients
)
DESCS=(
  "LLM calls outside the model layer (llm/, router/, provider/, modelport/)"
  "Memory mutations outside the memory layer (memory/, core/, recall/)"
  "Kernel entry points outside the UI manager"
  "Concrete implementation packages imported by orchestrator"
  "HTTP clients constructed outside safeurl/"
)
PATTERNS=(
  '\.(ChatCompletion|ChatCompletionStream|CreateEmbedding)\('
  '\.(EpisodicAdd|SemanticAdd|ProceduralAdd|Relate)\(|\.AddNode\(&core\.KGNode|\.AddEdge\(&core\.KGEdge'
  '(kernel|Kernel)\.Execute\('
  '"github.com/darkcode/(llm|memory|compression)"'
  'http\.DefaultClient|&http\.Client\{'
)
EXCLUDES=(
  '(^|/)(llm|router|provider|modelport)/'
  '(^|/)(memory|core|recall)/'
  ''
  '^\./(llm|memory|compression)/'
  '(^|/)safeurl/'
)
# orchestrator-impl-imports only applies within orchestrator/
RESTRICTS=( '' '' '' '^\./orchestrator/' '' )

# unwired_setters lists exported Kernel Set* methods that no non-test code
# calls. Such a method is a feature switch with no way to reach it: the
# reviewer shipped this way, 173 lines wired into the execute path whose only
# setter was called from reviewer_test.go, so it could not run in a real
# binary. go vet cannot see this — an exported method is "used" by definition.
unwired_setters() {
  local m callers
  grep -hoE '^func \(k \*Kernel\) (Set[A-Za-z0-9_]+)\(' orchestrator/*.go 2>/dev/null \
    | sed -E 's/^func \(k \*Kernel\) //; s/\($//' | sort -u \
  | while read -r m; do
      [ -z "$m" ] && continue
      callers=$(grep -rn --include='*.go' --exclude-dir='.claude' "\.$m(" . 2>/dev/null \
        | grep -v '_test\.go:' \
        | grep -v "^\./orchestrator/.*func (k \*Kernel) $m" \
        | grep -vE "^\./orchestrator/[a-z_]+\.go:[0-9]+:func " \
        | grep -c . )
      [ "$callers" -eq 0 ] && echo "orchestrator: Kernel.$m has no non-test caller"
    done
  true
}

# unbounded_completions counts model calls that send no MaxTokens.
#
# This is the boundary that actually costs money. Eight completion sites sent
# no ceiling, so their limit was whatever the provider defaults to — usually
# the rest of the context window — and they included the ReAct loop's main
# call, every sub-agent worker turn, every conversational answer, and the
# permission judge that decides whether a dangerous tool runs.
#
# A line count cannot see this: the ceiling is a field in a request built
# several lines away. So each call site is checked against its surrounding
# request instead.
unbounded_completions() {
  grep -rn --include='*.go' --exclude-dir='.claude' -E '\.(ChatCompletion|ChatCompletionStream)\(' . 2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -vE '(^|/)(llm|router|provider|modelport)/' \
    | cut -d: -f1,2 \
    | while IFS=: read -r file line; do
        start=$(( line > 22 ? line - 22 : 1 ))
        if ! sed -n "${start},$((line + 12))p" "$file" 2>/dev/null | grep -q 'MaxTokens'; then
          echo "$file:$line: model call with no token ceiling"
        fi
      done
  true
}

# unwired_managers lists manager packages whose constructor nothing outside
# tests calls. A manager with tests and no caller is a feature switch with no
# way to reach it — the reviewer shipped exactly that way, and modelport was
# built the same way in this very migration: a full package, well tested, and
# New() had no non-test caller, so only its policy table was reachable.
unwired_managers() {
  for pkg in modelport recall uiport spill planwork concurrency datasource; do
    [ -d "$pkg" ] || continue
    grep -qE "^func New\\(" "$pkg"/*.go 2>/dev/null || continue
    n=$(grep -rn --include='*.go' --exclude-dir='.claude' "$pkg\\.New(" . 2>/dev/null | grep -v '_test\.go:' | grep -vc "^\\./$pkg/")
    [ "$n" -eq 0 ] && echo "$pkg: New() has no non-test caller — the manager is unreachable"
  done
  true
}

declare -A CURRENT
for i in "${!NAMES[@]}"; do
  out="$(scan "${PATTERNS[$i]}" "${EXCLUDES[$i]}")"
  if [ -n "${RESTRICTS[$i]}" ]; then
    out="$(printf '%s\n' "$out" | grep -E "${RESTRICTS[$i]}")"
  fi
  # printf of an empty string still yields one blank line; count real lines only.
  count="$(printf '%s' "$out" | grep -c . || true)"
  CURRENT["${NAMES[$i]}"]="$count"
  if [ "$MODE" = "list" ]; then
    echo "── ${NAMES[$i]} ($count) — ${DESCS[$i]}"
    [ "$count" -gt 0 ] && printf '%s\n' "$out" | sed 's/^/   /'
    echo
  fi
done

# Eighth boundary: a manager nobody builds.
unwiredmgr_out="$(unwired_managers)"
unwiredmgr_count="$(printf '%s' "$unwiredmgr_out" | grep -c . || true)"
CURRENT["unwired-managers"]="$unwiredmgr_count"
NAMES+=(unwired-managers)
DESCS+=("Manager packages whose constructor nothing calls")
if [ "$MODE" = "list" ]; then
  echo "── unwired-managers ($unwiredmgr_count) — ${DESCS[-1]}"
  [ "$unwiredmgr_count" -gt 0 ] && printf '%s\n' "$unwiredmgr_out" | sed 's/^/   /'
  echo
fi

# Seventh boundary, computed by inspecting each call's request.
unbounded_out="$(unbounded_completions)"
unbounded_count="$(printf '%s' "$unbounded_out" | grep -c . || true)"
CURRENT["unbounded-completions"]="$unbounded_count"
NAMES+=(unbounded-completions)
DESCS+=("Model calls that send no MaxTokens")
if [ "$MODE" = "list" ]; then
  echo "── unbounded-completions ($unbounded_count) — ${DESCS[-1]}"
  [ "$unbounded_count" -gt 0 ] && printf '%s\n' "$unbounded_out" | sed 's/^/   /'
  echo
fi

# Sixth boundary, computed differently (call-graph, not a line count).
unwired_out="$(unwired_setters)"
unwired_count="$(printf '%s' "$unwired_out" | grep -c . || true)"
CURRENT["unwired-kernel-setters"]="$unwired_count"
NAMES+=(unwired-kernel-setters)
DESCS+=("Exported Kernel setters that nothing outside tests calls")
if [ "$MODE" = "list" ]; then
  echo "── unwired-kernel-setters ($unwired_count) — ${DESCS[-1]}"
  [ "$unwired_count" -gt 0 ] && printf '%s\n' "$unwired_out" | sed 's/^/   /'
  echo
fi

[ "$MODE" = "list" ] && exit 0

if [ "$MODE" = "update" ]; then
  {
    echo "# Architectural boundary baselines. See scripts/arch-check.sh."
    echo "# These are CEILINGS: a count may fall, never rise. Ratchet down as"
    echo "# the migration removes violations. A boundary at 0 is enforced."
    echo "# Regenerate with: scripts/arch-check.sh --update"
    echo
    for i in "${!NAMES[@]}"; do
      echo "# ${DESCS[$i]}"
      echo "${NAMES[$i]} ${CURRENT[${NAMES[$i]}]}"
    done
  } > "$BASELINE_FILE"
  echo "wrote $BASELINE_FILE"
  exit 0
fi

if [ ! -f "$BASELINE_FILE" ]; then
  echo "arch-check: $BASELINE_FILE missing. Create it with: scripts/arch-check.sh --update" >&2
  exit 2
fi

fail=0
ratchet=0
while read -r name limit; do
  [ -z "${name:-}" ] && continue
  case "$name" in \#*) continue ;; esac
  cur="${CURRENT[$name]:-}"
  if [ -z "$cur" ]; then
    echo "arch-check: baseline names unknown boundary '$name'" >&2
    fail=1
    continue
  fi
  if [ "$cur" -gt "$limit" ]; then
    echo "FAIL  $name: $cur violations, baseline allows $limit"
    echo "      A new violation of this boundary was introduced."
    echo "      Inspect with: scripts/arch-check.sh --list"
    fail=1
  elif [ "$cur" -lt "$limit" ]; then
    echo "RATCHET  $name: down to $cur from $limit — lower the baseline to lock it in"
    ratchet=1
  else
    echo "ok    $name: $cur (at baseline)"
  fi
done < "$BASELINE_FILE"

# A boundary that is computed but absent from the baseline is never checked,
# because the loop above iterates the baseline. That is the same defect these
# boundaries exist to catch, in the checker itself: it happened while adding
# unwired-managers, which silently did nothing until this guard was written.
for name in "${NAMES[@]}"; do
  if ! grep -qE "^${name} " "$BASELINE_FILE"; then
    echo "FAIL  ${name}: computed but missing from $BASELINE_FILE, so it is never checked"
    echo "      Run: scripts/arch-check.sh --update"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "arch-check failed. These boundaries are what keep the layering one-way."
  exit 1
fi
if [ "$ratchet" -ne 0 ]; then
  echo
  echo "Progress made. Run: scripts/arch-check.sh --update && git add $BASELINE_FILE"
fi
exit 0
