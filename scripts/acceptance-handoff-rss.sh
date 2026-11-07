#!/usr/bin/env bash
# Handoff dual-process RSS acceptance: two runtimes coexist inside a 2GB cgroup
# while 1000 SSE sessions run, the old one hands off mid-run and exits; the
# combined footprint must stay inside the budget.
#
# Usage: scripts/acceptance-handoff-rss.sh
# Requires: docker and a dockerized Postgres named $KEEL_PG_CTR (override with
# KEEL_PG_CTR; default keel-pg).
#
# Fail-closed: unreadable cgroup memory.peak, a peak above the ceiling, a stale
# (not freshly created) database, sessions != 1000, no text load, or any non-429
# SSE error exits non-zero. The merged result (loadtest fields + acceptance
# fields) is written to $KEEL_HANDOFF_OUT (default
# /tmp/perf-handoff-acceptance.json).
set -euo pipefail

DB="${KEEL_HANDOFF_RSS_DB:-keel_perf_handoff_e2e}"
CTR="${KEEL_HANDOFF_CTR:-keel-perf-handoff}"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
ADMIN_KEY="${KEEL_HANDOFF_ADMIN_KEY:-perf-handoff-key}"
DSN_BASE="${KEEL_HANDOFF_DSN_BASE:-postgres://postgres:runtime@host.docker.internal:13741}"
BENCH=/tmp/perf-handoff-bench.json
LOADLOG=/tmp/perf-handoff-loadtest.log
JSON_OUT="${KEEL_HANDOFF_OUT:-/tmp/perf-handoff-acceptance.json}"
CEILING_MB="${KEEL_HANDOFF_CEILING_MB:-1500}"
EXPECTED_SESSIONS="${KEEL_HANDOFF_EXPECTED_SESSIONS:-1000}"
EXPECTED_DURATION_SEC="${KEEL_HANDOFF_EXPECTED_DURATION_SEC:-60}"
D1="$DSN_BASE/$DB?sslmode=disable"

say() { printf '%s\n' "$*"; }

case "$CEILING_MB" in
  ''|*[!0-9]*) echo "FATAL: KEEL_HANDOFF_CEILING_MB must be a positive integer (got \"$CEILING_MB\")"; exit 1 ;;
esac
case "$EXPECTED_SESSIONS" in
  ''|*[!0-9]*) echo "FATAL: KEEL_HANDOFF_EXPECTED_SESSIONS must be a positive integer (got \"$EXPECTED_SESSIONS\")"; exit 1 ;;
esac
[ "$CEILING_MB" -gt 0 ] || { echo "FATAL: KEEL_HANDOFF_CEILING_MB must be > 0"; exit 1; }
[ "$EXPECTED_SESSIONS" -gt 0 ] || { echo "FATAL: KEEL_HANDOFF_EXPECTED_SESSIONS must be > 0"; exit 1; }

mkdir -p "$(dirname "$JSON_OUT")"

# Interlock: the script creates and drops this database. Refuse names that do
# not look dedicated so a mistyped environment variable cannot wipe a shared
# database (the same guard cmd/r1diff and cmd/e2e use).
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *e2e*|*test*|*smoke*|*perf*|*accept*) ;;
  *) echo "FATAL: refusing to use database \"$DB\": the name must look dedicated"; exit 1 ;;
esac

cleanup() {
  local code=$?
  docker rm -f "$CTR" >/dev/null 2>&1 || true
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT

docker rm -f "$CTR" >/dev/null 2>&1 || true
docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null \
  || { say "FATAL: could not drop database $DB (is the $PG_CTR container running?)"; exit 1; }
docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE $DB" >/dev/null \
  || { say "FATAL: could not create database $DB"; exit 1; }
TABLES=$(docker exec "$PG_CTR" psql -U postgres -d "$DB" -tAc \
  "SELECT count(*) FROM pg_tables WHERE schemaname='public'" | tr -d '[:space:]')
if [ "$TABLES" != "0" ]; then
  say "FATAL: database $DB is not fresh ($TABLES public tables) — refusing to run against stale data"
  exit 1
fi

docker run -d --name "$CTR" --memory=2g --memory-swap=2g --cpus=4 \
  -v /tmp/keel-perf-runtime:/runtime:ro -v /tmp/keel-perf-cas:/cas \
  -e "KEEL_DSN=$D1" -e GOMEMLIMIT=1610612736 -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 sh -c '/runtime -addr 127.0.0.1:13750 -cas /cas -streams 2000 -runs 64 >/tmp/a.log 2>&1 &
    sleep 1
    KEEL_RUNTIME_ID=runtime-new exec /runtime -addr 127.0.0.1:13751 -cas /cas -streams 2000 -runs 64 >/tmp/b.log 2>&1' >/dev/null

ready=0
for _ in $(seq 1 150); do
  if docker exec "$CTR" wget -qO- http://127.0.0.1:13750/v1/health >/dev/null 2>&1 && \
     docker exec "$CTR" wget -qO- http://127.0.0.1:13751/v1/health >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.2
done
if [ "$ready" != "1" ]; then
  docker logs "$CTR" 2>&1 | tail -20 || true
  say "FATAL: both runtimes did not become healthy"
  exit 1
fi
health_pid() {
  docker exec "$CTR" wget -qO- "http://127.0.0.1:$1/v1/health" 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid") or "")' 2>/dev/null || true
}
A_PID=$(health_pid 13750)
B_PID=$(health_pid 13751)
if [ -z "$A_PID" ] || [ -z "$B_PID" ] || [ "$A_PID" = "$B_PID" ]; then
  say "FATAL: could not identify two distinct runtime pids (old=${A_PID:-?} new=${B_PID:-?})"
  exit 1
fi
say "both healthy: old pid=$A_PID new pid=$B_PID"

docker run --rm --network "container:$CTR" -v /tmp/keel-perf-loadtest:/loadtest:ro -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 /loadtest -base http://127.0.0.1:13750 -sessions 1000 -duration 60s \
  -text-every 10s -image-every 60s -image-size 1048576 -images 20 -client-rate 51200 \
  > "$BENCH" 2> "$LOADLOG" &
LOAD_PID=$!
sleep 15
say "== drain old (handoff) while the load runs =="
docker exec "$CTR" sh -c "curl -s -XPOST http://127.0.0.1:13750/v1/runtime/drain -H 'Authorization: Bearer $ADMIN_KEY' -H 'Content-Type: application/json' -d '{\"mode\":\"handoff\",\"deadlineMs\":15000}'" 2>/dev/null || true
if ! wait "$LOAD_PID"; then
  say "WARN: loadtest exited non-zero (see $LOADLOG)"
fi

# memory.peak is the cgroup's own high-water mark; there is no memory.current
# fallback because the last reading would understate the dual-process peak.
PEAK=$(docker exec "$CTR" cat /sys/fs/cgroup/memory.peak 2>/dev/null | tr -d '\r' || true)
say "cgroup peak (both runtimes): ${PEAK:-UNREADABLE} bytes"
docker exec "$CTR" sh -c 'pgrep -f /runtime | wc -l; tail -3 /tmp/a.log; tail -3 /tmp/b.log' 2>/dev/null || true

python3 - "$BENCH" "$PEAK" "$JSON_OUT" "$CEILING_MB" "$EXPECTED_SESSIONS" "$EXPECTED_DURATION_SEC" <<'PY'
import json, os, sys

bench_path, peak_raw, out_path, ceiling_mb, expected_sessions, req_duration = sys.argv[1:7]
ceiling_mb = float(ceiling_mb)
expected_sessions = int(expected_sessions)
expected_duration = float(req_duration)
failures = []

try:
    with open(bench_path) as fh:
        bench = json.load(fh)
    if not isinstance(bench, dict):
        raise ValueError(f"expected a JSON object, got {type(bench).__name__}")
except Exception as exc:  # unreadable result is a failure, not an empty pass
    bench = {}
    failures.append(f"loadtest JSON unreadable ({bench_path}): {exc}")

if peak_raw.isdigit():
    peak = int(peak_raw)
else:
    peak = None
    failures.append(
        f"cgroup memory.peak unreadable (got {peak_raw!r}); refusing to certify an unknown peak"
    )

status = bench.get("statusCounts") or {}
throttled = int(status.get("429", 0))
sse_errors = int(bench.get("sseErrors", 0))
# Keep the acceptance criterion: 429 is documented admission backpressure and
# is retried by the loadtest; anything else in sseErrors is a hard failure.
hard = sse_errors - throttled

sessions = bench.get("sessions")
texts = bench.get("texts")
duration = bench.get("durationSec", 0)

if sessions != expected_sessions:
    failures.append(f"sessions = {sessions}, want exactly {expected_sessions} (empty-run guard)")
if not isinstance(texts, int) or texts <= 0:
    failures.append(f"texts = {texts}, want > 0 (no text load means the run was idle)")
# A short run would pass every count above and still show a low peak: the
# duration and the image channel must prove the budget was actually exercised.
if not isinstance(duration, (int, float)) or duration < 0.9 * expected_duration:
    failures.append(f"durationSec = {duration}, want >= {0.9 * expected_duration:.1f} of {expected_duration:.0f}s (short run)")
images_ok = bench.get("imagesOK")
if not isinstance(images_ok, int) or images_ok <= 0:
    failures.append(f"imagesOK = {images_ok}, want > 0 (the image channel was never exercised)")
if hard != 0:
    failures.append(f"hard SSE errors = {hard} (sseErrors {sse_errors} - 429 {throttled}), want 0")
if peak is not None and peak / 1e6 > ceiling_mb:
    failures.append(f"cgroup peak {peak / 1e6:.0f} MB exceeds the {ceiling_mb:.0f} MB ceiling")

summary = dict(bench)  # keep every loadtest field
summary.update({
    "mode": "handoff",
    "peakMB": round(peak / 1e6, 1) if peak is not None else None,
    "peakBytes": peak,
    "hardErrors": hard,
    "sseErrors": sse_errors,
    "statusCounts429": throttled,
    "sessions": sessions,
    "texts": texts,
    "durationSec": round(duration, 3) if isinstance(duration, (int, float)) else duration,
    "ceilingMB": ceiling_mb,
    "expectedSessions": expected_sessions,
    "verdict": "FAIL" if failures else "PASS",
    "failures": failures,
})

os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
with open(out_path, "w") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
    fh.write("\n")

print(json.dumps({k: summary.get(k) for k in (
    "mode", "sessions", "texts", "durationSec", "sseErrors", "statusCounts429",
    "hardErrors", "imagesOK", "imagesErr", "peakMB", "peakBytes", "ceilingMB", "verdict",
)}, ensure_ascii=False, indent=2))
if peak is None:
    print("cgroup peak with both runtimes: UNREADABLE (failing closed)")
else:
    print(f"cgroup peak with both runtimes: {peak/1e6:.0f} MB (ceiling {ceiling_mb:.0f} MB)")
for failure in failures:
    print(f"ACCEPTANCE FAIL: {failure}")
print(f"result JSON: {out_path}")
if failures:
    sys.exit(1)
print("verdict: PASS (dual-process peak inside the 2GB budget, 0 hard stream errors)")
PY
say "== done (container and database removed) =="
