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
# EXCLUDE_DIRS_REGEX, and strips comment lines. The comment strip matters: a
# comment mentioning http.DefaultClient is documentation, not a construction,
# and counting it produces a false violation (this exact false positive was hit
# while writing these baselines).
scan() {
  local pattern="$1" exclude="$2"
  grep -rn --include='*.go' -E "$pattern" . 2>/dev/null \
    | grep -v '_test\.go:' \
    | { [ -n "$exclude" ] && grep -vE "$exclude" || cat; } \
    | awk -F: '{ line=$0; sub(/^[^:]*:[0-9]+:/, "", line);
                 sub(/^[ \t]+/, "", line);
                 if (line !~ /^\/\// && line !~ /^\*/ && line !~ /^\/\*/) print }'
}

# Boundary definitions. Keep NAME, PATTERN and EXCLUDE aligned by index.
NAMES=(
  llm-calls
  memory-writes
  kernel-entry
  orchestrator-impl-imports
  raw-http-clients
)
DESCS=(
  "LLM calls outside the model layer (llm/, router/, provider/)"
  "Memory mutations outside the memory layer (memory/, core/)"
  "Kernel entry points outside the UI manager"
  "Concrete implementation packages imported by orchestrator"
  "HTTP clients constructed outside safeurl/"
)
PATTERNS=(
  '\.(ChatCompletion|ChatCompletionStream|CreateEmbedding)\('
  '\.(EpisodicAdd|SemanticAdd|ProceduralAdd|Relate|RecordFeedback|RecordAction)\(|\.AddNode\(&core\.KGNode|\.AddEdge\(&core\.KGEdge'
  '(kernel|Kernel)\.Execute\('
  '"github.com/darkcode/(llm|memory|compression)"'
  'http\.DefaultClient|&http\.Client\{'
)
EXCLUDES=(
  '(^|/)(llm|router|provider)/'
  '(^|/)(memory|core)/'
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
      callers=$(grep -rn --include='*.go' "\.$m(" . 2>/dev/null \
        | grep -v '_test\.go:' \
        | grep -v "^\./orchestrator/.*func (k \*Kernel) $m" \
        | grep -vE "^\./orchestrator/[a-z_]+\.go:[0-9]+:func " \
        | grep -c . )
      [ "$callers" -eq 0 ] && echo "orchestrator: Kernel.$m has no non-test caller"
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
