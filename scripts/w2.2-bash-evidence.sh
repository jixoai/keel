#!/usr/bin/env bash
# W2.2 (bash main bridge + JobManager) slice evidence.
#
# Runs the slice's gates and writes the raw transcripts under
# openspec/changes/keel-harness-v2/evidence/w2.2-bash/. Nothing is echoed from
# the environment: the test DSN and serve password are read into shell
# variables only.
#
# Usage: scripts/w2.2-bash-evidence.sh [--evidence DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
EVIDENCE="${KEEL_W22_EVIDENCE:-$ROOT/openspec/changes/keel-harness-v2/evidence/w2.2-bash}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
mkdir -p "$EVIDENCE"

log() { printf '[w2.2-evidence] %s\n' "$*"; }
fail() { printf '[w2.2-evidence] FAIL: %s\n' "$*" >&2; exit 1; }
record() { printf '%s\n' "$*" >> "$EVIDENCE/SUMMARY.md"; }

# --- test database ---------------------------------------------------------
if [[ -z "${KEEL_TEST_DSN:-}" ]]; then
  for candidate in "$ROOT/.env.local" "$ROOT/../../.env.personal"; do
    [[ -f "$candidate" ]] || continue
    value="$(grep '^KEEL_TEST_DSN_W2_BASH=' "$candidate" | cut -d= -f2- || true)"
    if [[ -z "$value" ]]; then
      value="$(grep '^KEEL_TEST_DSN=' "$candidate" | cut -d= -f2- || true)"
    fi
    if [[ -n "$value" ]]; then
      export KEEL_TEST_DSN="$value"
      break
    fi
  done
fi
[[ -n "${KEEL_TEST_DSN:-}" ]] || fail "KEEL_TEST_DSN (or KEEL_TEST_DSN_W2_BASH) is not set"

: > "$EVIDENCE/SUMMARY.md"
record "# W2.2 bash bridge + JobManager evidence"
record ""
record "- generated: $STAMP"
record "- base commit: $(git -C "$ROOT" rev-parse HEAD)"
record ""

run_gate() { # NAME COMMAND...
  local name="$1"; shift
  log "$name"
  if "$@" > "$EVIDENCE/$name.log" 2>&1; then
    record "- PASS $name"
  else
    record "- FAIL $name (see $name.log)"
    tail -20 "$EVIDENCE/$name.log" >&2 || true
    fail "$name"
  fi
}

cd "$ROOT"

# --- build / static gates --------------------------------------------------
run_gate build go build ./...
run_gate vet go vet ./internal/bashbridge ./internal/jobqueue ./internal/store ./internal/toolsurface ./internal/agentext ./cmd/keel

# --- package tests (real output, real test DB) -----------------------------
run_gate bashbridge-test go test ./internal/bashbridge -count=1 -v
run_gate jobqueue-test go test ./internal/jobqueue -count=1 -v
run_gate toolsurface-test go test ./internal/toolsurface -count=1 -v -run 'Bash|BuiltinSurface|ManifestHash|UnknownAction|ReservedAction'
run_gate agentext-shell-test go test ./internal/agentext -count=1 -v -run 'Shell'
run_gate store-test go test ./internal/store -count=1 -v -run 'BashJobs|ArchiveSession'
run_gate race-test go test -race ./internal/bashbridge ./internal/jobqueue ./internal/toolsurface ./internal/agentext -count=1

# --- contract assertions ---------------------------------------------------
log "contract assertions"
{
  echo "# gbash import points (must be internal/bashbridge only)"
  go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./... | grep 'ewhauser/gbash' | grep -v '^github.com/jixoai/keel/internal/bashbridge ' || true
  echo
  echo "# os/exec imports in the gbash adapter (must be empty)"
  go list -f '{{join .Imports "\n"}}' ./internal/bashbridge | grep -x 'os/exec' || true
  echo "# os/exec in the adapter's transitive deps (must be empty)"
  go list -deps ./internal/bashbridge | grep -x 'os/exec' || true
  echo
  echo "# ExecutionRequest.Timeout assignments in the adapter (must be empty)"
  grep -rn 'Timeout:' internal/bashbridge --include='*.go' || true
  echo
  echo "# pinned module"
  go list -m github.com/ewhauser/gbash
  grep '^github.com/ewhauser/gbash v0.0.41' go.sum | head -2
  echo
  echo "# host-binary / network / path assertions are executed by bashbridge-test:"
  echo "  TestSandboxHasNoHostBinariesAndNoNetwork"
  echo "  TestHostPathsAreUnreachable"
  echo "  TestWorkspaceAndCommandInjection"
  echo
  echo "# timeout/200MB/restart/orphan/limit assertions are executed by jobqueue-test:"
  echo "  TestTimeoutContinuesAsJobAndKeepsOutput"
  echo "  Test200MBOutputIsBoundedAndBlobReferenced"
  echo "  TestRestartConvergesRunningJobsToInterrupted"
  echo "  TestOrphanJobsAreCanceledWhenSessionArchived"
  echo "  TestLimitsRejectNewJobsWithoutKillingExisting"
} > "$EVIDENCE/contract-assertions.log" 2>&1

if grep -q '^os/exec$' "$EVIDENCE/contract-assertions.log"; then
  record "- FAIL contract-assertions (os/exec in adapter)"
  fail "the gbash adapter imports os/exec"
fi
if sed -n '1,/^$/p' "$EVIDENCE/contract-assertions.log" | \
    awk '/ewhauser\/gbash/ && $1 != "github.com/jixoai/keel/internal/bashbridge:" {found=1} END {exit !found}'; then
  record "- FAIL contract-assertions (gbash imported outside internal/bashbridge)"
  fail "gbash is imported outside internal/bashbridge"
fi
if grep -q '^Timeout:' "$EVIDENCE/contract-assertions.log"; then
  record "- FAIL contract-assertions (ExecutionRequest.Timeout assigned)"
  fail "the adapter assigns ExecutionRequest.Timeout"
fi
record "- PASS contract-assertions"

# --- product e2e (tool-surface constants must survive) ---------------------
log "agent-runtime-e2e"
if ./scripts/agent-runtime-e2e.sh --evidence "$EVIDENCE/agent-runtime" > "$EVIDENCE/agent-runtime-e2e.log" 2>&1; then
  record "- PASS agent-runtime-e2e"
else
  record "- FAIL agent-runtime-e2e (see agent-runtime-e2e.log)"
  tail -20 "$EVIDENCE/agent-runtime-e2e.log" >&2 || true
  fail "agent-runtime-e2e"
fi

record ""
record "All gates passed."
log "PASS — evidence in $EVIDENCE"
