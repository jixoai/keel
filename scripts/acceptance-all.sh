#!/usr/bin/env bash
# acceptance-all.sh —— 终轮全量验收（串行、fail-fast、证据落盘）
#
# Runs every gate the keel-service-graph delivery claims, in order, and drops
# raw evidence into $KEEL_EVIDENCE_DIR:
#   race-full.log            go test -race ./... (with command header)
#   conformance.json         scripts/conformance.sh (the 673 caliber, with DSN)
#   conformance-summary.txt  its human summary
#   r1-live.txt, r1/         the R1 capture (baseline e7361f8 vs candidate)
#   r1-negative.txt          stale-rule negative control (must FAIL)
#   e2e-7of7.json            cmd/e2e on a freshly created database
#   drain-smoke.txt          SIGTERM drain (health during drain, 503, SSE frame)
#   inflight.txt             input-during-run probe
#   external-smoke.log       external gateway end to end (DSH reference sidecar)
#   perf/chaos-kill.json     strict chaos, kill mode
#   perf/chaos-handoff.json  strict chaos, handoff mode (epoch evidence)
#   perf/final-1000.json     2GB cgroup / 1000 sessions (peak merged in)
#   perf/handoff-dual.json   dual-process handoff RSS (peak merged in)
#   acceptance-all.log       the run log with one verdict line per gate
#
# Usage:
#   KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=<dir> scripts/acceptance-all.sh
#
# Environment: KEEL_PG_CTR (dockerized Postgres, default keel-pg),
# KEEL_PERF_DIR / KEEL_PERF_DB / KEEL_HANDOFF_* passthrough (see the scripts).
# The load generators own their dedicated databases (this script drops and
# recreates them first, WITH (FORCE)).
set -euo pipefail
cd "$(dirname "$0")/.."

: "${KEEL_TEST_DSN:?set KEEL_TEST_DSN (PostgreSQL DSN for the test suites)}"
: "${KEEL_EVIDENCE_DIR:?set KEEL_EVIDENCE_DIR (where the raw evidence lands)}"

EVID="$KEEL_EVIDENCE_DIR"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
ADMIN_KEY="${KEEL_ADMIN_KEY:-keel-acceptance-admin}"
BIN=/tmp/keel-runtime-acceptance
BASE=/tmp/keel-r1-base
LOG="$EVID/acceptance-all.log"
mkdir -p "$EVID/perf"
: > "$LOG"

say() { printf '%s\n' "$*" | tee -a "$LOG"; }
fail() { say "FAIL: $*"; exit 1; }

# run <label> <logfile> <cmd...> — a gate passes only on exit 0.
run() {
  local label="$1" out="$2"; shift 2
  say "-- $label"
  if "$@" >>"$out" 2>&1; then
    say "PASS: $label ($out)"
  else
    say "FAIL: $label (exit $?) — tail of $out:"
    tail -25 "$out" | sed 's/^/    /' | tee -a "$LOG"
    exit 1
  fi
}

# dsn <db> — a dedicated DSN on the dockerized Postgres.
dsn() { printf 'postgres://postgres:runtime@127.0.0.1:13741/%s?sslmode=disable' "$1"; }

# db_reset <db> — drop (forced) and recreate a dedicated database.
db_reset() {
  docker exec "$PG_CTR" psql -U postgres -q \
    -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" -c "CREATE DATABASE $1" >/dev/null \
    || fail "could not reset database $1 (is the $PG_CTR container running?)"
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
run "go build ./..." "$EVID/build-vet.log" go build ./...
run "go vet ./..." "$EVID/build-vet.log" go vet ./...
run "build candidate runtime" "$EVID/build-vet.log" go build -o "$BIN" ./cmd/runtime

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
run "build R1 baseline (e7361f8)" "$EVID/r1-build.txt" \
  scripts/build-r1-baseline.sh e7361f8 "$BASE"
port_free 13778
port_free 13779
run "R1 live capture" "$EVID/r1-live.txt" \
  go run ./cmd/r1diff -old-bin "$BASE" -new-bin "$BIN" \
  -dsn "$(dsn keel_r1diff)" -out "$EVID/r1" \
  -allow openspec/changes/archive/keel-service-graph/r1-allow.json \
  -port-old 13778 -port-new 13779
# Negative control: the same dump on both sides means every rule went stale —
# the gate must refuse to pass (it is not allowed to pass vacuously).
say "-- R1 negative control (identical dumps must FAIL)"
if go run ./cmd/r1diff -old-dump "$EVID/r1/new.json" -new-dump "$EVID/r1/new.json" \
  -allow openspec/changes/archive/keel-service-graph/r1-allow.json \
  -out "$EVID/r1-negative-control" >"$EVID/r1-negative.txt" 2>&1; then
  fail "R1 negative control passed: the gate can be satisfied without its differences"
fi
say "PASS: R1 negative control refused (see $EVID/r1-negative.txt)"

# --- 4. E2E on a fresh database --------------------------------------------
db_reset keel_e2e_final
port_free 13745
run "E2E (fresh database)" "$EVID/e2e-7of7.json" \
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

say "ALL GATES PASS — $(date)"
