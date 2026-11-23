#!/usr/bin/env bash
# acceptance-all.sh —— 终轮全量验收（串行、fail-fast、证据落盘）
#
# Runs every gate the keel-service-graph delivery claims, in order, and drops
# raw evidence into $KEEL_EVIDENCE_DIR:
#   race-full.log            go test -race ./... (with command header)
#   conformance.json         scripts/conformance.sh (the 673 caliber, with DSN)
#   conformance-summary.txt  its human summary
#   r1-live.txt, r1/         the R1 capture (pinned baseline from cmd/r1diff/baseline.json vs candidate)
#   r1-negative.txt          stale-rule negative control (must FAIL)
#   e2e-8of8.json            cmd/e2e on a freshly created database
#   drain-smoke.txt          SIGTERM drain (health during drain, 503, SSE frame)
#   inflight.txt             input-during-run probe
#   external-smoke.log       external gateway end to end (DSH reference sidecar)
#   perf/chaos-kill.json     strict chaos, kill mode
#   perf/chaos-handoff.json  strict chaos, handoff mode (epoch evidence)
#   perf/final-1000.json     2GB cgroup / 1000 sessions (peak merged in)
#   perf/handoff-dual.json   dual-process handoff RSS (peak merged in)
#   perf/rss-gate.json       VmRSS process-tree gate, only when opted in (see §8b)
#   acceptance-all.log       the run log with one verdict line per gate
#
# Usage:
#   KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=<dir> scripts/acceptance-all.sh
#
# Environment: KEEL_TEST_DSN is the maintenance connection; its host/user/
# query carry over to every dedicated database this script creates.
# KEEL_PSQL (default psql) names the PostgreSQL client binary;
# KEEL_ACCEPTANCE_MAINTENANCE_DSN overrides the connection used for DROP/CREATE.
# KEEL_PERF_DIR / KEEL_PERF_DB / KEEL_HANDOFF_* passthrough (see the scripts).
# The load generators own their dedicated databases (this script drops and
# recreates them first, WITH (FORCE)). No Docker is required.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${KEEL_TEST_DSN:?set KEEL_TEST_DSN (PostgreSQL DSN for the test suites)}"
: "${KEEL_EVIDENCE_DIR:?set KEEL_EVIDENCE_DIR (where the raw evidence lands)}"

EVID="$KEEL_EVIDENCE_DIR"
PSQL="${KEEL_PSQL:-psql}"
MAINTENANCE_DSN="${KEEL_ACCEPTANCE_MAINTENANCE_DSN:-$KEEL_TEST_DSN}"
ADMIN_KEY="${KEEL_ADMIN_KEY:-keel-acceptance-admin}"
BIN=/tmp/keel-runtime-acceptance
BASE=/tmp/keel-r1-base
LOG="$EVID/acceptance-all.log"
mkdir -p "$EVID/perf"
: > "$LOG"

say() { printf '%s\n' "$*" | tee -a "$LOG"; }
fail() { say "FAIL: $*"; exit 1; }

# run <label> <logfile> <cmd...> — a gate passes only on exit 0. The artifact
# is truncated first: re-running the battery in the same directory must never
# leave two runs' output concatenated (a stale artifact is a lying artifact).
run() {
  local label="$1" out="$2"; shift 2
  say "-- $label"
  : >"$out"
  if "$@" >>"$out" 2>&1; then
    say "PASS: $label ($out)"
  else
    say "FAIL: $label (exit $?) — tail of $out:"
    tail -25 "$out" | sed 's/^/    /' | tee -a "$LOG"
    exit 1
  fi
}

# run_split <label> <out> <errlog> <cmd...> — like run, but stdout and stderr
# land in separate files: a JSON artifact must stay a single parseable document
# even when the tool logs progress to stderr.
run_split() {
  local label="$1" out="$2" errlog="$3"; shift 3
  say "-- $label"
  : >"$out"
  : >"$errlog"
  if "$@" >>"$out" 2>>"$errlog"; then
    say "PASS: $label ($out)"
  else
    say "FAIL: $label (exit $?) — tail of $out:"
    tail -25 "$out" | sed 's/^/    /' | tee -a "$LOG"
    say "    (stderr: $errlog)"
    tail -10 "$errlog" | sed 's/^/    /' | tee -a "$LOG"
    exit 1
  fi
}

# dsn <db> — a dedicated database on the same server and credentials as
# KEEL_TEST_DSN. Only the database path changes; host/user/query are preserved.
dsn() {
  local base="${KEEL_TEST_DSN%%\?*}"
  local query=""
  if [[ "$KEEL_TEST_DSN" == *\?* ]]; then
    query="?${KEEL_TEST_DSN#*\?}"
  fi
  printf '%s/%s%s' "${base%/*}" "$1" "$query"
}

# db_reset <db> — drop (forced) and recreate a dedicated database through the
# local PostgreSQL client. A missing psql/server is a hard failure.
db_reset() {
  "$PSQL" "$MAINTENANCE_DSN" -v ON_ERROR_STOP=1 -q \
    -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" -c "CREATE DATABASE $1" >/dev/null \
    || fail "could not reset database $1 (psql=$PSQL maintenance=$MAINTENANCE_DSN)"
}

# port_free <port> — refuse to run when a stale server holds the port.
port_free() {
  if lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; then
    fail "port $1 is already in use: $(lsof -nP -iTCP:"$1" -sTCP:LISTEN | tail -1)"
  fi
}

say "keel acceptance battery — $(date)"
say "evidence: $EVID"

# --- 0. the tree itself -----------------------------------------------------
run "go build ./..." "$EVID/build.log" go build ./...
run "go vet ./..." "$EVID/vet.log" go vet ./...
run "build candidate runtime" "$EVID/runtime-build.log" go build -o "$BIN" ./cmd/runtime
# The catalog is the only machine-readable coverage truth; COVERAGE.md §A/§B/§C
# are mapped onto it and §H is generated (docs/PLUGIN-CATALOG.md).
run "plugin catalog" "$EVID/plugin-catalog.txt" node scripts/plugin-catalog.mjs --check
# G1's static half: every tabulated state names an authority fact (no memory-only
# state); the output is the state -> fact mapping the acceptance gate asks for.
run "state -> fact mapping" "$EVID/state-facts.txt" node scripts/state-facts.mjs

# --- 1. the full race suite -------------------------------------------------
{
  echo "\$ go test -race -count=1 ./..."
  echo "# KEEL_TEST_DSN is set (the PostgreSQL paths run, not skip)"
  echo "# started $(date)"
  go test -race -count=1 ./...
} >"$EVID/race-full.log" 2>&1 || fail "go test -race ./... (tail: $(tail -3 "$EVID/race-full.log"))"
if grep -q 'DATA RACE\|^FAIL' "$EVID/race-full.log"; then
  fail "the race log contains FAIL/DATA RACE"
fi
say "PASS: go test -race ./... ($EVID/race-full.log)"

# --- 2. PI conformance ------------------------------------------------------
run "conformance" "$EVID/conformance-summary.txt" \
  scripts/conformance.sh "$EVID/conformance.json"

# --- 3. R1 differential against the frozen baseline -------------------------
run "build R1 baseline (pin)" "$EVID/r1-build.txt" \
  scripts/build-r1-baseline.sh
db_reset keel_r1diff
port_free 13778
port_free 13779
run "R1 live capture" "$EVID/r1-live.txt" \
  go run ./cmd/r1diff -old-bin "$BASE" -new-bin "$BIN" \
  -dsn "$(dsn keel_r1diff)" -out "$EVID/r1" \
  -pin cmd/r1diff/baseline.json \
  -allow cmd/r1diff/allow-v2.json \
  -port-old 13778 -port-new 13779
# Negative control: the same dump on both sides means every rule went stale —
# the gate must refuse to pass (it is not allowed to pass vacuously).
say "-- R1 negative control (identical dumps must FAIL)"
if go run ./cmd/r1diff -old-dump "$EVID/r1/new.json" -new-dump "$EVID/r1/new.json" \
  -pin cmd/r1diff/baseline.json -allow cmd/r1diff/allow-v2.json \
  -out "$EVID/r1-negative-control" >"$EVID/r1-negative.txt" 2>&1; then
  fail "R1 negative control passed: the gate can be satisfied without its differences"
fi
say "PASS: R1 negative control refused (see $EVID/r1-negative.txt)"

# --- 4. E2E on a fresh database --------------------------------------------
db_reset keel_e2e_final
port_free 13745
run_split "E2E (fresh database)" "$EVID/e2e-8of8.json" "$EVID/e2e-run.log" \
  go run ./cmd/e2e -bin "$BIN" -dsn "$(dsn keel_e2e_final)" -port 13745 \
  -reset -reset-confirm keel_e2e_final

# --- 5. drain + in-flight ---------------------------------------------------
db_reset keel_drain_smoke
run "SIGTERM drain" "$EVID/drain-smoke.txt" env KEEL_DRAIN_BIN="$BIN" scripts/acceptance-drain.sh
run "in-flight input probe" "$EVID/inflight.txt" env KEEL_INFLIGHT_BIN="$BIN" scripts/acceptance-inflight.sh

# --- 6. external gateway end to end ----------------------------------------
run "external gateway smoke" "$EVID/external-smoke.log" scripts/external-gateway-smoke.sh

# --- 7. chaos: kill + handoff ----------------------------------------------
db_reset keel_chaos_final
port_free 13795
port_free 13796
run "chaos kill (strict)" "$EVID/perf/chaos-kill.log" \
  go run ./cmd/chaos -bin "$BIN" -dsn "$(dsn keel_chaos_final)" -admin-key "$ADMIN_KEY" \
  -mode kill -faults 10 -port 13795 -sessions 6 -duration 120s -kill-every 4s -settle 20s \
  -out "$EVID/perf/chaos-kill.json"
db_reset keel_chaos_final
run "chaos handoff (strict)" "$EVID/perf/chaos-handoff.log" \
  go run ./cmd/chaos -bin "$BIN" -dsn "$(dsn keel_chaos_final)" -admin-key "$ADMIN_KEY" \
  -mode handoff -faults 6 -port 13793 -sessions 6 -duration 120s -kill-every 4s -settle 20s \
  -out "$EVID/perf/chaos-handoff.json"

# --- 8. the memory budgets --------------------------------------------------
run "2GB / 1000 sessions" "$EVID/perf/final-1000.log" \
  env KEEL_PERF_DB=keel_perf_final KEEL_PERF_OUT="$EVID/perf/final-1000.json" \
  scripts/acceptance-perf.sh
run "handoff dual-process RSS" "$EVID/perf/handoff-dual.log" \
  env KEEL_HANDOFF_RSS_DB=keel_perf_handoff_final KEEL_HANDOFF_OUT="$EVID/perf/handoff-dual.json" \
  scripts/acceptance-handoff-rss.sh

# --- 8b. VmRSS process-tree gate (opt-in) -----------------------------------
# The 2GB cgroup peak above answers "did it survive the hard limit"; this gate
# answers the harness-v2 memory protocol (sum of VmRSS over the backend tree,
# <=500MB hard, <=300MB target, acceptance.md §0). It is off by default because
# it starts its own native runtime plus a 1000-session load (~2 minutes and a
# dedicated database), which would make every local run heavier.
#   KEEL_ACCEPTANCE_RSS=1        run the profile gate (needs KEEL_TEST_DSN)
#   KEEL_ACCEPTANCE_RSS=attach   sample an already-running backend instead;
#                                requires KEEL_RSS_PORT or KEEL_RSS_PID
# On a platform with neither procfs nor ps the sampler exits UNMEASURABLE (2)
# and this gate fails; it never passes without a real measurement.
case "${KEEL_ACCEPTANCE_RSS:-0}" in
  1|profile)
    rm -f "$EVID/perf/rss-gate.json"  # a stale JSON must not survive a failed run
    run "VmRSS process-tree gate (profile)" "$EVID/perf/rss-gate.log" \
      env KEEL_RSS_OUT="$EVID/perf/rss-gate.json" KEEL_RSS_DB=keel_perf_rss KEEL_RSS_DIR=/tmp/keel-rss-acceptance \
      scripts/acceptance-rss.sh --profile
    ;;
  attach)
    if [ -z "${KEEL_RSS_PORT:-}" ] && [ -z "${KEEL_RSS_PID:-}" ]; then
      fail "KEEL_ACCEPTANCE_RSS=attach needs KEEL_RSS_PORT or KEEL_RSS_PID"
    fi
    rm -f "$EVID/perf/rss-gate.json"
    run "VmRSS process-tree gate (attach)" "$EVID/perf/rss-gate.log" \
      env KEEL_RSS_OUT="$EVID/perf/rss-gate.json" KEEL_RSS_DIR=/tmp/keel-rss-acceptance \
      scripts/acceptance-rss.sh
    ;;
  *)
    say "SKIP: VmRSS process-tree gate (set KEEL_ACCEPTANCE_RSS=1 for the 1000-session profile, or =attach with KEEL_RSS_PORT/KEEL_RSS_PID)"
    ;;
esac

# --- 9. real model E2E (needs credentials + network; opt-in) ----------------
# The provider-configuration phase is the only gate that proves the tenant's
# provider document reached the wire (model id, reasoning level, max tokens,
# credential). It cannot be hermetic: it talks to a real gateway through a
# recording proxy. Off by default; on with KEEL_ACCEPTANCE_REAL_MODEL=1 when
# credentials are configured (scripts/real-model-e2e.sh --check reports them).
if [ "${KEEL_ACCEPTANCE_REAL_MODEL:-0}" = "1" ]; then
  run "real model E2E (round trip)" "$EVID/real-model.txt" scripts/real-model-e2e.sh
  run "real model E2E (provider configuration)" "$EVID/real-model-models.txt" \
    scripts/real-model-e2e.sh --models
else
  say "SKIP: real model E2E (set KEEL_ACCEPTANCE_REAL_MODEL=1 with credentials to include it)"
fi

say "ALL GATES PASS — $(date)"
