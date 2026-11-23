#!/usr/bin/env bash
# acceptance-rss.sh — the VmRSS process-tree memory gate (W0 RSS sampler / W11 G4).
#
# Protocol (openspec/changes/keel-harness-v2 acceptance §0):
#   RSS = sum of VmRSS over the backend process tree (root + descendants),
#   sampled every <=10s, excluding page cache. Thresholds: >500MB fails;
#   300-500MB passes as "above target"; <=300MB is on target.
#
# Two modes, both fail-closed:
#
#   attach (default)  sample an already-running backend:
#       scripts/acceptance-rss.sh --port 13740
#       scripts/acceptance-rss.sh --pid 12345 --duration 10s
#     The port is resolved through the backend's own /v1/health and cross-checked
#     with lsof, so a proxy/foreign listener is rejected instead of measured.
#
#   profile           start a dedicated native runtime and drive the documented
#                     workload (1000 SSE sessions, 64 concurrent runs, 1MiB
#                     image channel), sample while it runs, then require both
#                     the RSS budget and the workload to be genuine:
#       KEEL_TEST_DSN='postgres://…' scripts/acceptance-rss.sh --profile
#     The runtime and load generator run natively so the measured tree is real;
#     the cgroup peak is a separate metric (acceptance-perf.sh / g5-perf), and
#     can be attached with --cgroup <container> when the backend runs in one.
#
# Measurement source: Linux /proc/<pid>/status:VmRSS; macOS ps RSS (the platform
# has no procfs/VmRSS). If neither exists the run ends UNMEASURABLE (exit 2),
# never PASS.
#
# Exit codes: 0 PASS (incl. above-target), 1 breach or invalid workload,
# 2 unmeasurable / identity failure / unusable configuration (the machine-readable
# summary and stderr line say which). A 2 is never "pass with a caveat".
#
# Usage:
#   scripts/acceptance-rss.sh [--port PORT | --pid PID] [flags]
#   scripts/acceptance-rss.sh --profile [flags]
#   scripts/acceptance-rss.sh --self-test
#
# Flags (env in parentheses):
#   --port N            KEEL_RSS_PORT        backend port for /v1/health identity
#   --pid N             KEEL_RSS_PID         backend root pid
#   --duration SEC      KEEL_RSS_DURATION    attach sampling window (default 10)
#   --interval SEC      KEEL_RSS_INTERVAL    sample interval 1..10 (default 1)
#   --ceiling-mb N      KEEL_RSS_CEILING_MB  hard ceiling (default 500)
#   --target-mb N       KEEL_RSS_TARGET_MB   target (default 300)
#   --min-samples N     KEEL_RSS_MIN_SAMPLES minimum samples (default 3)
#   --out FILE          KEEL_RSS_OUT         merged summary JSON (default /tmp/keel-rss/summary.json)
#   --dir DIR           KEEL_RSS_DIR         profile work dir (default /tmp/keel-rss)
#   --cgroup NAME       KEEL_RSS_CGROUP      optional container to read memory.peak from
#   --profile           KEEL_RSS_PROFILE=1   run the 1000-session profile
#   --sessions N        KEEL_RSS_SESSIONS    profile SSE sessions (default 1000)
#   --runs N            KEEL_RSS_RUNS        profile concurrent runs (default 64)
#   --load-duration SEC KEEL_RSS_LOAD_SEC    profile load window (default 60)
#   --images N          KEEL_RSS_IMAGES      profile seeded images (default 20)
#   --image-size BYTES  KEEL_RSS_IMAGE_SIZE  profile image size (default 1MiB)
#   --text-every DUR    KEEL_RSS_TEXT_EVERY  profile text period (default 1h)
#   --client-rate BPS   KEEL_RSS_CLIENT_RATE profile per-stream read rate (default 51200)
#   --drain DUR         KEEL_RSS_DRAIN       profile wait-drain cap (default 90s)
#   --db NAME           KEEL_RSS_DB          profile database (default keel_rss_perf)
#   --dsn DSN           KEEL_RSS_DSN / KEEL_TEST_DSN  profile PostgreSQL base DSN
#   --runtime-bin PATH  KEEL_RSS_RUNTIME_BIN profile runtime binary (default /tmp/keel-rss-runtime)
#   --load-bin PATH     KEEL_RSS_LOAD_BIN    profile loadtest binary (default /tmp/keel-rss-loadtest)
#   --admin-key KEY     KEEL_ADMIN_KEY       profile operator key
#   --fake-delay-ms MS  KEEL_RSS_FAKE_DELAY_MS  profile fake provider delay (default 30000)
#   --pg-ctr NAME       KEEL_PG_CTR          dockerized PG fallback when psql is absent
#   --allow-no-images   KEEL_RSS_ALLOW_NO_IMAGES=1  do not require the image channel
#   --self-test         run the parser/sum/threshold fixtures and exit
set -euo pipefail
cd "$(dirname "$0")/.."

SELF=scripts/lib/rss_sampler.py
TEST=scripts/lib/test_rss_sampler.py

PROFILE="${KEEL_RSS_PROFILE:-0}"
PID="${KEEL_RSS_PID:-}"
PORT="${KEEL_RSS_PORT:-}"
DURATION="${KEEL_RSS_DURATION:-10}"
INTERVAL="${KEEL_RSS_INTERVAL:-1}"
CEILING_MB="${KEEL_RSS_CEILING_MB:-500}"
TARGET_MB="${KEEL_RSS_TARGET_MB:-300}"
MIN_SAMPLES="${KEEL_RSS_MIN_SAMPLES:-3}"
OUT_JSON="${KEEL_RSS_OUT:-/tmp/keel-rss/summary.json}"
WORK_DIR="${KEEL_RSS_DIR:-/tmp/keel-rss}"
CGROUP="${KEEL_RSS_CGROUP:-}"
SESSIONS="${KEEL_RSS_SESSIONS:-1000}"
RUNS="${KEEL_RSS_RUNS:-64}"
LOAD_SEC="${KEEL_RSS_LOAD_SEC:-60}"
IMAGES="${KEEL_RSS_IMAGES:-20}"
IMAGE_SIZE="${KEEL_RSS_IMAGE_SIZE:-1048576}"
TEXT_EVERY="${KEEL_RSS_TEXT_EVERY:-1h}"
CLIENT_RATE="${KEEL_RSS_CLIENT_RATE:-51200}"
DRAIN="${KEEL_RSS_DRAIN:-90s}"
DB="${KEEL_RSS_DB:-keel_rss_perf}"
BASE_DSN="${KEEL_RSS_DSN:-${KEEL_TEST_DSN:-}}"
RUNTIME_BIN="${KEEL_RSS_RUNTIME_BIN:-/tmp/keel-rss-runtime}"
LOAD_BIN="${KEEL_RSS_LOAD_BIN:-/tmp/keel-rss-loadtest}"
ADMIN_KEY="${KEEL_ADMIN_KEY:-rss-gate-admin}"
FAKE_DELAY_MS="${KEEL_RSS_FAKE_DELAY_MS:-30000}"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
ALLOW_NO_IMAGES="${KEEL_RSS_ALLOW_NO_IMAGES:-0}"
PROFILE_PORT="${KEEL_RSS_PROFILE_PORT:-13770}"
SELF_TEST=0

say() { printf '%s\n' "$*" >&2; }
fatal() { say "RSS GATE FATAL: $*"; exit 2; }
usage() { sed -n '2,70p' "$0" | grep '^#' | sed 's/^# \{0,1\}//'; }

need_value() {
  [ -n "${2:-}" ] || fatal "flag $1 requires a value"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --profile) PROFILE=1 ;;
    --pid) need_value "$1" "${2:-}"; PID="$2"; shift ;;
    --port) need_value "$1" "${2:-}"; PORT="$2"; shift ;;
    --duration) need_value "$1" "${2:-}"; DURATION="$2"; shift ;;
    --interval) need_value "$1" "${2:-}"; INTERVAL="$2"; shift ;;
    --ceiling-mb) need_value "$1" "${2:-}"; CEILING_MB="$2"; shift ;;
    --target-mb) need_value "$1" "${2:-}"; TARGET_MB="$2"; shift ;;
    --min-samples) need_value "$1" "${2:-}"; MIN_SAMPLES="$2"; shift ;;
    --out) need_value "$1" "${2:-}"; OUT_JSON="$2"; shift ;;
    --dir) need_value "$1" "${2:-}"; WORK_DIR="$2"; shift ;;
    --cgroup) need_value "$1" "${2:-}"; CGROUP="$2"; shift ;;
    --sessions) need_value "$1" "${2:-}"; SESSIONS="$2"; shift ;;
    --runs) need_value "$1" "${2:-}"; RUNS="$2"; shift ;;
    --load-duration) need_value "$1" "${2:-}"; LOAD_SEC="$2"; shift ;;
    --images) need_value "$1" "${2:-}"; IMAGES="$2"; shift ;;
    --image-size) need_value "$1" "${2:-}"; IMAGE_SIZE="$2"; shift ;;
    --text-every) need_value "$1" "${2:-}"; TEXT_EVERY="$2"; shift ;;
    --client-rate) need_value "$1" "${2:-}"; CLIENT_RATE="$2"; shift ;;
    --drain) need_value "$1" "${2:-}"; DRAIN="$2"; shift ;;
    --db) need_value "$1" "${2:-}"; DB="$2"; shift ;;
    --dsn) need_value "$1" "${2:-}"; BASE_DSN="$2"; shift ;;
    --runtime-bin) need_value "$1" "${2:-}"; RUNTIME_BIN="$2"; shift ;;
    --load-bin) need_value "$1" "${2:-}"; LOAD_BIN="$2"; shift ;;
    --admin-key) need_value "$1" "${2:-}"; ADMIN_KEY="$2"; shift ;;
    --fake-delay-ms) need_value "$1" "${2:-}"; FAKE_DELAY_MS="$2"; shift ;;
    --pg-ctr) need_value "$1" "${2:-}"; PG_CTR="$2"; shift ;;
    --allow-no-images) ALLOW_NO_IMAGES=1 ;;
    --self-test) SELF_TEST=1 ;;
    -h|--help) usage; exit 0 ;;
    *) fatal "unknown argument: $1 (see --help)" ;;
  esac
  shift
done

if [ "$SELF_TEST" = "1" ]; then
  exec python3 -I "$TEST"
fi

# Seconds flags accept either "10" or "10s"; strip the suffix before the
# integer validation so both spellings behave identically. Each strip applies
# to the already-stripped value (``${X%S}`` on the original would undo it).
DURATION="${DURATION%s}"
DURATION="${DURATION%S}"
LOAD_SEC="${LOAD_SEC%s}"
LOAD_SEC="${LOAD_SEC%S}"
DRAIN_SEC="${DRAIN%s}"
DRAIN_SEC="${DRAIN_SEC%S}"
case "$DRAIN_SEC" in
  ''|*[!0-9]*) fatal "--drain must be seconds like 90s (got \"$DRAIN\")" ;;
esac
for pair in "duration:$DURATION" "interval:$INTERVAL" "ceiling-mb:$CEILING_MB" "target-mb:$TARGET_MB" \
            "min-samples:$MIN_SAMPLES" "sessions:$SESSIONS" "runs:$RUNS" "load-duration:$LOAD_SEC"; do
  name=${pair%%:*}; value=${pair#*:}
  case "$value" in
    ''|*[!0-9]*) fatal "--$name must be a positive integer number of seconds (got \"$value\")" ;;
  esac
  [ "$value" -gt 0 ] || fatal "--$name must be > 0"
done
[ "$TARGET_MB" -le "$CEILING_MB" ] || fatal "--target-mb cannot exceed --ceiling-mb"
[ "$INTERVAL" -le 10 ] || fatal "--interval must be <= 10s (sampling protocol)"
[ "$MIN_SAMPLES" -ge 2 ] || fatal "--min-samples must be >= 2 (a single reading is not a window)"
# samples land at t=0, interval, 2*interval, ... and possibly exactly at the
# window end, so a DURATION-second window yields DURATION/INTERVAL + 1.
EXPECTED_SAMPLES=$((DURATION / INTERVAL + 1))
if [ "$PROFILE" != "1" ] && [ "$EXPECTED_SAMPLES" -lt "$MIN_SAMPLES" ]; then
  fatal "--duration $DURATION with --interval $INTERVAL yields ~$EXPECTED_SAMPLES sample(s); raise --duration or lower --min-samples (need >= $MIN_SAMPLES)"
fi

mkdir -p "$WORK_DIR" "$(dirname "$OUT_JSON")"

SAMPLER_ARGS="--interval $INTERVAL --ceiling-mb $CEILING_MB --target-mb $TARGET_MB --min-samples $MIN_SAMPLES"

port_is_free() {
  if command -v lsof >/dev/null 2>&1; then
    ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  else
    python3 -I -c '
import socket, sys
s = socket.socket()
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
' "$1"
  fi
}

# attach_mode ---------------------------------------------------------------
attach_mode() {
  local out="$OUT_JSON" log="$WORK_DIR/sampler.log" rc=0
  if [ -n "$PID" ] && [ -n "$PORT" ]; then
    fatal "give either --pid or --port, not both"
  fi
  if [ -z "$PID" ] && [ -z "$PORT" ]; then
    fatal "attach mode needs --port or --pid (or use --profile)"
  fi
  if [ -z "$PID" ] && ! port_is_free "$PORT"; then
    say "attach: port $PORT is listening; resolving its owner via /v1/health + lsof"
  fi
  mkdir -p "$(dirname "$log")"
  local target_args
  if [ -n "$PID" ]; then target_args=(--pid "$PID"); else target_args=(--port "$PORT"); fi
  # shellcheck disable=SC2086
  python3 -I "$SELF" sample "${target_args[@]}" --duration "$DURATION" $SAMPLER_ARGS \
    --mode attach --out "$out" >"$WORK_DIR/attach.stdout" 2>"$log" || rc=$?
  cat "$WORK_DIR/attach.stdout"
  say "$(cat "$log")"
  if [ "$rc" -eq 0 ]; then attach_cgroup; else say "RSS GATE: cgroup peak not attached (sampler exit $rc)"; fi
  exit "$rc"
}

# profile_mode --------------------------------------------------------------
RT_PID=''
SAMP_PID=''
STOP_FILE="$WORK_DIR/stop-sampling"
DB_CREATED=0
BENCH="$WORK_DIR/bench.json"
SAMPLER_SUMMARY="$WORK_DIR/sampler.json"
PROFILE_LOG="$WORK_DIR/profile.log"

cleanup() {
  local code=$?
  if [ -n "$SAMP_PID" ]; then
    kill -9 "$SAMP_PID" 2>/dev/null || true
    wait "$SAMP_PID" 2>/dev/null || true
  fi
  if [ -n "$RT_PID" ]; then
    kill -TERM "$RT_PID" 2>/dev/null || true
    for _ in $(seq 1 100); do kill -0 "$RT_PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$RT_PID" 2>/dev/null || true
    wait "$RT_PID" 2>/dev/null || true
    if kill -0 "$RT_PID" 2>/dev/null; then
      say "RSS GATE: FATAL: runtime pid $RT_PID survived cleanup"
    fi
  fi
  if [ "$DB_CREATED" = "1" ]; then
    drop_database >/dev/null 2>&1 || say "RSS GATE: WARNING: could not drop database $DB"
  fi
  if [ "$PROFILE" = "1" ] && ! port_is_free "$PROFILE_PORT"; then
    say "RSS GATE: WARNING: port $PROFILE_PORT is still listening after cleanup"
  fi
  exit "$code"
}
trap cleanup EXIT

dsn_with_db() {
  python3 -I -c '
import sys, urllib.parse
u = urllib.parse.urlparse(sys.argv[1])
print(urllib.parse.urlunparse(u._replace(path="/" + sys.argv[2])))
' "$BASE_DSN" "$1"
}

sql_exec() { # sql_exec <database> <sql...>; prints psql output
  local database="$1"; shift
  if command -v psql >/dev/null 2>&1; then
    PGCONNECT_TIMEOUT=10 psql "$(dsn_with_db "$database")" -v ON_ERROR_STOP=1 -q "$@"
  elif docker_ready; then
    run_bounded 30 docker exec "$PG_CTR" psql -U postgres -v ON_ERROR_STOP=1 -q -d "$database" "$@"
  else
    return 127
  fi
}

# bounded: the docker CLI can block indefinitely when the daemon is wedged; a
# probe must fail fast instead of hanging the gate (macOS has no coreutils
# timeout, so the watchdog is a background kill loop).
run_bounded() { # run_bounded <seconds> <cmd...>
  local seconds="$1"; shift
  local pid rc=0
  "$@" &
  pid=$!
  for _ in $(seq 1 "$((seconds * 10))"); do
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid" || rc=$?
      return "$rc"
    fi
    sleep 0.1
  done
  kill -9 "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  say "RSS GATE: timed out after ${seconds}s: $*"
  return 124
}

docker_ready() {
  command -v docker >/dev/null 2>&1 || return 1
  run_bounded 15 docker exec "$PG_CTR" true >/dev/null 2>&1
}

drop_database() {
  if command -v psql >/dev/null 2>&1; then
    run_bounded 30 psql "$(dsn_with_db postgres)" -v ON_ERROR_STOP=1 -q \
      -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null
  else
    run_bounded 30 docker exec "$PG_CTR" psql -U postgres -v ON_ERROR_STOP=1 -q \
      -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null
  fi
}

create_database() {
  if command -v psql >/dev/null 2>&1; then
    run_bounded 30 psql "$(dsn_with_db postgres)" -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$DB\"" >/dev/null
  else
    run_bounded 30 docker exec "$PG_CTR" psql -U postgres -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$DB\"" >/dev/null
  fi
}

profile_mode() {
  [ -n "$BASE_DSN" ] || fatal "--profile needs KEEL_TEST_DSN / KEEL_RSS_DSN (or --dsn)"
  case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
    *test*|*perf*|*accept*|*e2e*|*smoke*) ;;
    *) fatal "refusing database \"$DB\": the name must look dedicated (test/perf/accept/e2e/smoke)" ;;
  esac
  [ "$DB" != "postgres" ] || fatal "refusing the postgres maintenance database"

  if [ -n "$PID" ] || [ -n "$PORT" ]; then
    fatal "--profile starts its own backend; do not pass --pid/--port"
  fi

  : >"$PROFILE_LOG"
  say "== VmRSS profile: $SESSIONS sessions / $RUNS runs / ${LOAD_SEC}s / db=$DB port=$PROFILE_PORT =="

  say "-- build runtime + loadtest (native, so the measured tree is real)"
  if [ "${KEEL_RSS_REUSE_BIN:-0}" = "1" ] && [ -x "$RUNTIME_BIN" ] && [ -x "$LOAD_BIN" ]; then
    say "   reusing binaries (KEEL_RSS_REUSE_BIN=1): $RUNTIME_BIN $LOAD_BIN"
  else
    go build -o "$RUNTIME_BIN" ./cmd/runtime >>"$PROFILE_LOG" 2>&1 \
      || fatal "go build ./cmd/runtime failed (see $PROFILE_LOG)"
    go build -o "$LOAD_BIN" ./cmd/loadtest >>"$PROFILE_LOG" 2>&1 \
      || fatal "go build ./cmd/loadtest failed (see $PROFILE_LOG)"
  fi

  say "-- fresh database $DB"
  if ! command -v psql >/dev/null 2>&1 && ! docker_ready; then
    fatal "no reachable PostgreSQL: install psql or start the dockerized $PG_CTR (docker daemon probe failed)"
  fi
  drop_database >>"$PROFILE_LOG" 2>&1 \
    || fatal "could not drop database $DB (need psql on PATH or a dockerized $PG_CTR)"
  create_database >>"$PROFILE_LOG" 2>&1 || fatal "could not create database $DB"
  DB_CREATED=1
  local tables
  tables=$(sql_exec "$DB" -tAc "SELECT count(*) FROM pg_tables WHERE schemaname='public'" 2>>"$PROFILE_LOG" | tr -d '[:space:]')
  [ "$tables" = "0" ] || fatal "database $DB is not fresh ($tables public tables)"

  port_is_free "$PROFILE_PORT" || fatal "port $PROFILE_PORT is already in use"

  say "-- start runtime (pid is verified through /v1/health)"
  local cas="$WORK_DIR/cas"
  mkdir -p "$cas"
  KEEL_DSN="$(dsn_with_db "$DB")" KEEL_ADMIN_KEY="$ADMIN_KEY" \
  KEEL_FAKE_DELAY_MS="$FAKE_DELAY_MS" \
    "$RUNTIME_BIN" -addr "127.0.0.1:$PROFILE_PORT" -cas "$cas" \
    -streams 2200 -runs "$RUNS" -chunk-batch 1 -admit-rate 1200 \
    >>"$PROFILE_LOG" 2>&1 &
  RT_PID=$!

  local healthy=0 health answered
  for _ in $(seq 1 300); do
    health=$(curl --noproxy '*' -fsS -m 2 "http://127.0.0.1:$PROFILE_PORT/v1/health" 2>/dev/null || true)
    if [ -n "$health" ]; then
      answered=$(printf '%s' "$health" | python3 -I -c 'import json,sys; print(json.load(sys.stdin).get("pid") or "")' 2>/dev/null || true)
      if [ "$answered" = "$RT_PID" ]; then healthy=1; break; fi
      fatal "port $PROFILE_PORT answers with pid ${answered:-?}, not the runtime we started ($RT_PID)"
    fi
    if ! kill -0 "$RT_PID" 2>/dev/null; then
      tail -30 "$PROFILE_LOG" >&2 || true
      fatal "runtime exited during startup"
    fi
    sleep 0.2
  done
  [ "$healthy" = "1" ] || fatal "runtime did not become healthy"
  say "   runtime pid=$RT_PID healthy; /v1/health pid matches"

  local sampler_window=$((LOAD_SEC + DRAIN_SEC + 60))
  say "-- start VmRSS sampler for pid $RT_PID (interval ${INTERVAL}s, window up to ${sampler_window}s)"
  rm -f "$STOP_FILE"
  # shellcheck disable=SC2086
  python3 -I "$SELF" sample --pid "$RT_PID" --duration "$sampler_window" $SAMPLER_ARGS \
    --stop-file "$STOP_FILE" --mode profile \
    --workload-sessions "$SESSIONS" --workload-load-duration "$LOAD_SEC" \
    --out "$SAMPLER_SUMMARY" >"$WORK_DIR/sampler.stdout" 2>"$WORK_DIR/sampler.log" &
  SAMP_PID=$!

  say "-- loadtest: $SESSIONS SSE sessions, $RUNS run-driving sessions, ${IMAGE_SIZE}B images"
  # Capture the exit code from a plain command: `if ! cmd; then rc=$?` sees 0
  # because the negation is the last command (the classic trap).
  local load_rc=0
  "$LOAD_BIN" -base "http://127.0.0.1:$PROFILE_PORT" -admin-key "$ADMIN_KEY" \
    -sessions "$SESSIONS" -duration "${LOAD_SEC}s" \
    -text-sessions "$RUNS" -prefill-runs "$RUNS" -text-every "$TEXT_EVERY" \
    -image-every 60s -image-size "$IMAGE_SIZE" -images "$IMAGES" \
    -client-rate "$CLIENT_RATE" -wait-drain "$DRAIN" \
    >"$BENCH" 2>"$WORK_DIR/loadtest.log" || load_rc=$?
  touch "$STOP_FILE"

  local sampler_rc=0
  wait "$SAMP_PID" || sampler_rc=$?
  SAMP_PID=''
  if [ "$load_rc" -ne 0 ]; then
    tail -40 "$WORK_DIR/loadtest.log" >&2 || true
    say "RSS GATE: loadtest failed (exit $load_rc); the merge records it as a workload failure"
  fi

  say "-- merge RSS + workload and enforce thresholds"
  if [ ! -s "$SAMPLER_SUMMARY" ]; then
    say "RSS GATE FATAL: sampler produced no summary (see $WORK_DIR/sampler.log)"
    exit 2
  fi
  local merge_rc=0
  local merge_args=(--summary "$SAMPLER_SUMMARY" --bench "$BENCH"
    --expected-sessions "$SESSIONS" --expected-runs "$RUNS"
    --expected-duration "$LOAD_SEC" --out "$OUT_JSON")
  [ "$ALLOW_NO_IMAGES" = "1" ] && merge_args+=(--allow-no-images)
  # shellcheck disable=SC2086
  python3 -I "$SELF" merge "${merge_args[@]}" >"$WORK_DIR/merge.stdout" 2>>"$PROFILE_LOG" || merge_rc=$?
  [ "$sampler_rc" -eq 0 ] || say "RSS GATE: NOTE: sampler exit $sampler_rc (see $WORK_DIR/sampler.log)"
  attach_cgroup
  cat "$WORK_DIR/merge.stdout"
  say "$(cat "$PROFILE_LOG")"
  exit "$merge_rc"
}

# attach_cgroup <summary-json> — read memory.peak from a container and attach it
# to the summary as a separate metric (never part of the RSS verdict).
attach_cgroup() {
  [ -n "$CGROUP" ] || return 0
  local raw peak
  if ! command -v docker >/dev/null 2>&1; then
    fatal "--cgroup $CGROUP needs docker"
  fi
  raw=$(run_bounded 20 docker exec "$CGROUP" cat /sys/fs/cgroup/memory.peak 2>/dev/null | tr -d '\r' || true)
  case "$raw" in
    ''|*[!0-9]*) fatal "cgroup memory.peak unreadable from $CGROUP (got \"$raw\"); refusing to report a fake peak" ;;
  esac
  peak="$raw"
  python3 -I -c '
import json, sys
path, peak = sys.argv[1], int(sys.argv[2])
with open(path, encoding="utf-8") as fh:
    summary = json.load(fh)
summary["cgroup"] = {
    "container": sys.argv[3],
    "peakBytes": peak,
    "peakMB": round(peak / 1e6, 2),
    "note": "memory.peak includes page cache and is reported separately from the VmRSS gate",
}
with open(path, "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
' "$OUT_JSON" "$peak" "$CGROUP"
  say "RSS GATE: cgroup peak ${peak} bytes ($(python3 -I -c 'import sys; print(round(int(sys.argv[1])/1e6,2))' "$peak") MB) from $CGROUP (separate from the RSS verdict)"
}

if [ "$PROFILE" = "1" ]; then
  profile_mode
else
  attach_mode
fi
