#!/usr/bin/env bash
# Generate the Phase 0 evidence for keel-board: run every gate against an
# immutable board commit, capture the real output, and write a markdown summary.
# The summary is generated, never hand-edited.
#
# Usage:
#   scripts/board-phase0-evidence.sh [--board-dir DIR] [--out DIR] [--with-stress]
#
# KEEL_BOARD_TEST_DSN must point at a PostgreSQL server the board tests may use.
# The DSN is passed to the gates through the environment and is never written to
# the evidence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOARD="${KEEL_BOARD_DIR:-$HOME/Dev/GitHub/jixoai/keel-board}"
OUT=""
STRESS=0

while [ $# -gt 0 ]; do
  case "$1" in
    --board-dir) BOARD="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --with-stress) STRESS=1; shift ;;
    -h|--help)
      sed -n '2,12p' "$0"
      exit 0
      ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

DSN="${KEEL_BOARD_TEST_DSN:-}"
if [ -z "$DSN" ]; then
  echo "KEEL_BOARD_TEST_DSN is required" >&2
  exit 2
fi
if [ ! -d "$BOARD/.git" ] && [ ! -f "$BOARD/.git" ]; then
  echo "not a git worktree: $BOARD" >&2
  exit 2
fi

if [ -z "$OUT" ]; then
  OUT="$ROOT/openspec/changes/keel-board-v1/evidence/phase-0-gates-$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$OUT"
LOG="$OUT/commands.log"
: > "$LOG"

commit="$(git -C "$BOARD" rev-parse HEAD)"
tag="$(git -C "$BOARD" describe --tags --exact-match "$commit" 2>/dev/null || true)"
branch="$(git -C "$BOARD" rev-parse --abbrev-ref HEAD)"
dirty="$(git -C "$BOARD" status --porcelain)"
module="$(cd "$BOARD" && go list -m)"
go_version="$(cd "$BOARD" && go version)"
psql_version="$(psql --version 2>/dev/null || echo 'psql not found')"

run_gate() {
  local name="$1"
  shift
  local started ended code
  started="$(date +%s)"
  printf '\n$ %s\n' "$*" >>"$LOG"
  set +e
  ( cd "$BOARD" && "$@" ) >>"$LOG" 2>&1
  code=$?
  set -e
  ended="$(date +%s)"
  printf '%s\t%s\t%s\n' "$name" "$code" "$((ended - started))" >>"$OUT/gates.tsv"
  return "$code"
}

: >"$OUT/gates.tsv"
failed=0
gate() {
  local name="$1"
  shift
  if ! run_gate "$name" "$@"; then
    failed=1
  fi
}

gate gofmt sh -c 'test -z "$(gofmt -l .)"'
gate vet go vet ./...
gate test env "KEEL_BOARD_TEST_DSN=$DSN" go test -count=1 ./...
gate race env "KEEL_BOARD_TEST_DSN=$DSN" go test -race -count=1 ./...
gate e2e go run ./scripts/e2e -report-dir "$OUT/e2e"
if [ "$STRESS" -eq 1 ]; then
  gate stress env "KEEL_BOARD_TEST_DSN=$DSN" go test -count=10 -timeout=30m ./store ./log ./syncstore ./automation ./conformance
fi

# The tag and a clean tree are part of the evidence, not a separate checklist.
if [ -n "$dirty" ]; then
  failed=1
fi
if [ -z "$tag" ]; then
  failed=1
fi
if [ ! -f "$BOARD/LICENSE" ] || [ ! -f "$BOARD/NOTICE" ]; then
  failed=1
fi

{
  printf '# keel-board Phase 0 evidence\n\n'
  printf 'Generated at %s by `scripts/board-phase0-evidence.sh`.\n\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '## Immutable revision\n\n'
  printf '| Field | Value |\n| --- | --- |\n'
  printf '| Repository | `%s` |\n' "$module"
  printf '| Branch | `%s` |\n' "$branch"
  printf '| Commit | `%s` |\n' "$commit"
  printf '| Tag | `%s` |\n' "${tag:-MISSING}"
  printf '| Worktree | %s |\n' "$([ -z "$dirty" ] && echo clean || echo DIRTY)"
  printf '| Go | `%s` |\n' "$go_version"
  printf '| PostgreSQL client | `%s` |\n' "$psql_version"
  printf '| License files | %s |\n' "$([ -f "$BOARD/LICENSE" ] && [ -f "$BOARD/NOTICE" ] && echo present || echo MISSING)"
  printf '\n## Gates\n\n'
  printf '| Gate | Exit | Seconds |\n| --- | ---: | ---: |\n'
  while IFS=$'\t' read -r name code seconds; do
    printf '| `%s` | %s | %s |\n' "$name" "$code" "$seconds"
  done <"$OUT/gates.tsv"
  printf '\nFull output: `commands.log`. E2E report: `e2e/report.html`.\n\n'
  printf '## Independent review\n\n'
  if [ -f "$OUT/phase-0-independent-review.md" ]; then
    printf 'Findings are recorded verbatim in `phase-0-independent-review.md`; the fix list and re-review live beside it.\n'
  else
    printf 'MISSING: `phase-0-independent-review.md` has not been placed in this directory.\n'
    failed=1
  fi
  printf '\n## Result\n\n'
  if [ "$failed" -eq 0 ]; then
    printf 'PASS: every gate exited 0, the tree is clean, the commit is tagged, and the license files exist.\n'
  else
    printf 'FAIL: at least one gate failed, the tree is dirty, the tag is missing, or the license files are missing.\n'
  fi
} >"$OUT/phase-0-gates.md"

if [ "$failed" -ne 0 ]; then
  echo "evidence written with failures: $OUT/phase-0-gates.md" >&2
  exit 1
fi
echo "evidence: $OUT/phase-0-gates.md"
