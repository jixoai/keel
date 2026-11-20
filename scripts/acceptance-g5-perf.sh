#!/usr/bin/env bash
# acceptance-g5-perf.sh — the G5 scale profile: 1000 SSE sessions + 64
# concurrent agent runs + periodic 1MiB image streams, all served by one real
# runtime process inside a 2GB cgroup.
#
# The profile is one combined run on purpose: the memory bar is only meaningful
# when every documented channel is active at the same time. The cgroup's own
# memory.peak is the high-water mark; an unreadable peak is a failed run.
#
# What it proves:
#   - exactly 1000 sessions hold an SSE stream (streamsActiveMax >= expected)
#   - exactly N runs execute concurrently (keel_runs_active == N) and all of
#     them converge (done, 0 failed, queue empty)
#   - 1MiB image streams are exercised with 429 admission retried
#   - cgroup memory.peak is readable, under the 2GB hard limit, and under the
#     MATURITY.md 1.8GB GA ceiling
#
# Usage:
#   scripts/acceptance-g5-perf.sh <evidence-dir>
#
# The DSN comes from KEEL_TEST_DSN or .env.local (never printed). The database
# is dropped and recreated; its name must look dedicated (test/perf/accept).
# Subscale dry runs require KEEL_G5_ALLOW_SUBSCALE=1; the formal gate refuses
# anything but 1000 sessions / 64 runs.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT_DIR="${1:-${KEEL_G5_PERF_OUT:-/tmp/keel-g5-perf}}"
mkdir -p "$OUT_DIR"

SESSIONS="${KEEL_G5_SESSIONS:-1000}"
RUNS="${KEEL_G5_RUNS:-64}"
DURATION="${KEEL_G5_DURATION:-60s}"
DURATION_SEC="${DURATION%s}"
TEXT_EVERY="${KEEL_G5_TEXT_EVERY:-1h}"
IMAGE_EVERY="${KEEL_G5_IMAGE_EVERY:-60s}"
IMAGE_SIZE="${KEEL_G5_IMAGE_SIZE:-1048576}"
IMAGES="${KEEL_G5_IMAGES:-20}"
CLIENT_RATE="${KEEL_G5_CLIENT_RATE:-51200}"
DRAIN="${KEEL_G5_DRAIN:-90s}"
CEILING_MB="${KEEL_G5_CEILING_MB:-1800}"
HARD_LIMIT_MB=2048
EXPECTED_SESSIONS="${KEEL_G5_EXPECTED_SESSIONS:-1000}"
EXPECTED_RUNS="${KEEL_G5_EXPECTED_RUNS:-64}"
FAKE_DELAY_MS="${KEEL_G5_FAKE_DELAY_MS:-30000}"
PORT="${KEEL_G5_PORT:-13750}"
ADMIN_KEY="${KEEL_G5_ADMIN_KEY:-g5-acceptance-admin}"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
CTR="${KEEL_G5_CTR:-keel-g5-2g}"
CAS="${KEEL_G5_CAS:-/tmp/keel-g5-cas}"
ENVFILE=""

say() { printf '%s\n' "$*"; }
fatal() { say "G5 PERF FATAL: $*"; exit 1; }

if [ "${KEEL_G5_ALLOW_SUBSCALE:-0}" != "1" ]; then
  [ "$SESSIONS" -eq 1000 ] || fatal "formal profile requires 1000 sessions (got $SESSIONS)"
  [ "$RUNS" -eq 64 ] || fatal "formal profile requires 64 concurrent runs (got $RUNS)"
  [ "$EXPECTED_SESSIONS" -eq 1000 ] || fatal "formal profile requires EXPECTED_SESSIONS=1000"
  [ "$EXPECTED_RUNS" -eq 64 ] || fatal "formal profile requires EXPECTED_RUNS=64"
fi
case "$(printf %s "$DURATION_SEC" | tr -d ' ')" in
  ''|*[!0-9]*) fatal "duration must look like 60s (got $DURATION)" ;;
esac

if [ -z "${KEEL_TEST_DSN:-}" ]; then
  [ -f .env.local ] || fatal "KEEL_TEST_DSN is unset and .env.local is missing"
  KEEL_TEST_DSN=$(python3 -I - <<'PY'
import re
for line in open('.env.local', encoding='utf-8'):
    m = re.match(r'\s*KEEL_TEST_DSN\s*=\s*(.*)$', line)
    if not m:
        continue
    v = m.group(1).strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
        v = v[1:-1]
    print(v)
    break
PY
)
fi
: "${KEEL_TEST_DSN:?set KEEL_TEST_DSN (or fill .env.local)}"

DB=$(python3 -I - "$KEEL_TEST_DSN" <<'PY'
import sys, urllib.parse
print(urllib.parse.urlparse(sys.argv[1]).path.lstrip('/'))
PY
)
DOCKER_DSN=$(python3 -I - "$KEEL_TEST_DSN" <<'PY'
import sys, urllib.parse
u = urllib.parse.urlparse(sys.argv[1])
netloc = u.netloc.replace('127.0.0.1', 'host.docker.internal').replace('localhost', 'host.docker.internal')
print(urllib.parse.urlunparse(u._replace(netloc=netloc)))
PY
)
[ -n "$DB" ] || fatal "could not read a database name from the DSN"
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *test*|*perf*|*accept*|*e2e*|*smoke*) ;;
  *) fatal "refusing database \"$DB\": name must look dedicated (test/perf/accept/e2e/smoke)" ;;
esac

docker exec "$PG_CTR" true >/dev/null 2>&1 || fatal "PostgreSQL container $PG_CTR is not running"
for p in "$PORT" "$((PORT + 1))"; do
  if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    fatal "port $p is already in use: $(lsof -nP -iTCP:"$p" -sTCP:LISTEN | tail -1)"
  fi
done

cleanup() {
  code=$?
  if docker inspect "$CTR" >/dev/null 2>&1; then
    docker logs "$CTR" >"$OUT_DIR/runtime.log" 2>&1 || true
    docker rm -f "$CTR" >/dev/null 2>&1 || true
  fi
  if [ -n "$ENVFILE" ]; then rm -f "$ENVFILE"; fi
  {
    echo "exit=$code"
    if docker exec "$PG_CTR" psql -U postgres -q -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1; then
      echo "database_dropped=$DB"
    else
      echo "database_drop=FAILED:$DB"
    fi
    if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
      echo "port_$PORT=STILL_LISTENING"
    else
      echo "port_$PORT=free"
    fi
    if lsof -nP -iTCP:"$((PORT + 1))" -sTCP:LISTEN >/dev/null 2>&1; then
      echo "port_$((PORT + 1))=STILL_LISTENING"
    else
      echo "port_$((PORT + 1))=free"
    fi
    if docker inspect "$CTR" >/dev/null 2>&1; then
      echo "container=STILL_PRESENT:$CTR"
    else
      echo "container=removed:$CTR"
    fi
  } >"$OUT_DIR/cleanup.txt"
  exit "$code"
}
trap cleanup EXIT

say "== G5 perf profile: $SESSIONS sessions / $RUNS runs / $DURATION / 2GB cgroup =="
say "   database=$DB port=$PORT fake_delay_ms=$FAKE_DELAY_MS ceiling=${CEILING_MB}MB"

say "-- build linux binaries"
CGO_ENABLED=0 GOOS=linux GOARCH="$(go env GOARCH)" go build -o /tmp/keel-g5-runtime ./cmd/runtime
CGO_ENABLED=0 GOOS=linux GOARCH="$(go env GOARCH)" go build -o /tmp/keel-g5-loadtest ./cmd/loadtest

say "-- fresh database $DB"
docker exec "$PG_CTR" psql -U postgres -q \
  -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" \
  -c "CREATE DATABASE \"$DB\"" >/dev/null \
  || fatal "could not reset database $DB"

mkdir -p "$CAS"
ENVFILE=$(mktemp /tmp/keel-g5-env.XXXXXX)
chmod 600 "$ENVFILE"
{
  echo "KEEL_DSN=$DOCKER_DSN"
  echo "GOMEMLIMIT=1610612736"
  echo "KEEL_ADMIN_KEY=$ADMIN_KEY"
  echo "KEEL_FAKE_DELAY_MS=$FAKE_DELAY_MS"
} >"$ENVFILE"

say "-- start runtime under --memory=2g"
docker run -d --name "$CTR" --memory=2g --memory-swap=2g --cpus=4 \
  -p "127.0.0.1:$PORT:$PORT" \
  -v /tmp/keel-g5-runtime:/runtime:ro -v "$CAS":/cas \
  --env-file "$ENVFILE" \
  alpine:3.20 /runtime -addr ":$PORT" -cas /cas -streams 2000 -runs "$RUNS" >/dev/null
rm -f "$ENVFILE"
ENVFILE=""

for _ in $(seq 1 150); do
  if curl --noproxy '*' -fsS "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl --noproxy '*' -fsS "http://127.0.0.1:$PORT/v1/health" >/dev/null \
  || { docker logs "$CTR" 2>&1 | tail -30; fatal "runtime did not become healthy"; }
MEM_MAX=$(docker exec "$CTR" cat /sys/fs/cgroup/memory.max | tr -d '\r')
[ "$MEM_MAX" = "2147483648" ] || fatal "cgroup memory.max is $MEM_MAX, want 2147483648 (2GB)"
say "   runtime healthy; cgroup memory.max=$MEM_MAX"

say "-- loadtest: $SESSIONS SSE sessions, $RUNS run-driving sessions, images ${IMAGE_SIZE}B every $IMAGE_EVERY"
if ! docker run --rm --network "container:$CTR" \
  -v /tmp/keel-g5-loadtest:/loadtest:ro \
  -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 /loadtest -base "http://127.0.0.1:$PORT" \
  -admin-key "$ADMIN_KEY" -sessions "$SESSIONS" -duration "$DURATION" \
  -text-sessions "$RUNS" -prefill-runs "$RUNS" -text-every "$TEXT_EVERY" \
  -image-every "$IMAGE_EVERY" -image-size "$IMAGE_SIZE" -images "$IMAGES" \
  -client-rate "$CLIENT_RATE" -wait-drain "$DRAIN" \
  >"$OUT_DIR/bench.json" 2>"$OUT_DIR/loadtest.log"; then
  tail -40 "$OUT_DIR/loadtest.log" || true
  fatal "loadtest failed (see $OUT_DIR/loadtest.log)"
fi

PEAK=$(docker exec "$CTR" sh -c 'cat /sys/fs/cgroup/memory.peak 2>/dev/null' | tr -d '\r' || true)
CURRENT=$(docker exec "$CTR" sh -c 'cat /sys/fs/cgroup/memory.current 2>/dev/null' | tr -d '\r' || true)
printf '%s\n' "$PEAK" >"$OUT_DIR/cgroup-memory-peak.txt"
printf '%s\n' "$MEM_MAX" >"$OUT_DIR/cgroup-memory-max.txt"
printf '%s\n' "$CURRENT" >"$OUT_DIR/cgroup-memory-current.txt"

say "-- assert profile"
python3 -I - "$OUT_DIR/bench.json" "$PEAK" "$MEM_MAX" "$OUT_DIR/result.json" \
  "$CEILING_MB" "$HARD_LIMIT_MB" "$EXPECTED_SESSIONS" "$EXPECTED_RUNS" "$DURATION_SEC" <<'PY'
import json
import sys

(bench_path, peak_raw, max_raw, out_path, ceiling_mb, hard_limit_mb,
 expected_sessions, expected_runs, expected_duration) = sys.argv[1:10]
ceiling_mb = float(ceiling_mb)
hard_limit_mb = float(hard_limit_mb)
expected_sessions = int(expected_sessions)
expected_runs = int(expected_runs)
expected_duration = float(expected_duration)
failures = []

try:
    with open(bench_path) as fh:
        bench = json.load(fh)
    if not isinstance(bench, dict):
        raise ValueError(f"expected an object, got {type(bench).__name__}")
except Exception as exc:
    bench = {}
    failures.append(f"loadtest JSON unreadable ({bench_path}): {exc}")

peak = int(peak_raw) if peak_raw.strip().isdigit() else None
mem_max = int(max_raw) if max_raw.strip().isdigit() else None
if peak is None:
    failures.append(f"cgroup memory.peak unreadable (got {peak_raw!r}); refusing to certify an unknown peak")
if mem_max != 2147483648:
    failures.append(f"cgroup memory.max = {mem_max}, want 2147483648 (2GB)")

status = bench.get("statusCounts") or {}
throttled = int(status.get("429", 0))
sse_errors = int(bench.get("sseErrors", 0))
hard = sse_errors - throttled

sessions = bench.get("sessions")
sse_sessions = bench.get("sseSessions")
texts = bench.get("texts")
duration = bench.get("durationSec", 0)
images_ok = bench.get("imagesOK")
streams_max = bench.get("streamsActiveMax")
runs_active_max = bench.get("runsActiveMax")
runs_started = bench.get("runsStartedFinal")
runs_done = bench.get("runsFinishedDone")
runs_failed = bench.get("runsFailedFinal")
runs_pending_final = bench.get("runsPendingFinal")
runs_active_final = bench.get("runsActiveFinal")
run_status_counts = bench.get("runStatusCounts") or {}
metrics_error = bench.get("metricsError") or ""

if metrics_error:
    failures.append(f"metrics sampler error: {metrics_error}")
if sessions != expected_sessions:
    failures.append(f"sessions = {sessions}, want exactly {expected_sessions}")
if sse_sessions != expected_sessions:
    failures.append(f"sseSessions = {sse_sessions}, want exactly {expected_sessions} (some sessions never established their SSE stream)")
if not isinstance(texts, int) or texts < expected_runs:
    failures.append(f"texts = {texts}, want >= {expected_runs} (the run-driving channels were idle)")
if not isinstance(duration, (int, float)) or duration < 0.9 * expected_duration:
    failures.append(f"durationSec = {duration}, want >= {0.9 * expected_duration:.1f}s of {expected_duration:.0f}s (short run)")
if not isinstance(images_ok, int) or images_ok <= 0:
    failures.append(f"imagesOK = {images_ok}, want > 0 (the image channel was never exercised)")
if hard != 0:
    failures.append(f"hard SSE errors = {hard} (sseErrors {sse_errors} - 429 {throttled}), want 0")
if not isinstance(streams_max, int) or streams_max < expected_sessions:
    failures.append(f"streamsActiveMax = {streams_max}, want >= {expected_sessions} (sessions never all online)")
if runs_active_max != expected_runs:
    failures.append(f"runsActiveMax = {runs_active_max}, want exactly {expected_runs} concurrent runs")
if not isinstance(runs_started, int) or runs_started < expected_runs:
    failures.append(f"runsStartedFinal = {runs_started}, want >= {expected_runs}")
if not isinstance(runs_done, int) or runs_done < expected_runs:
    failures.append(f"runsFinishedDone = {runs_done}, want >= {expected_runs}")
if runs_failed not in (0, None):
    failures.append(f"runsFailedFinal = {runs_failed}, want 0")
if runs_pending_final not in (0, None):
    failures.append(f"runsPendingFinal = {runs_pending_final}, want 0 (queue never converged)")
if runs_active_final not in (0, None):
    failures.append(f"runsActiveFinal = {runs_active_final}, want 0")
if run_status_counts:
    done = int(run_status_counts.get("done", 0))
    if done != expected_runs:
        failures.append(f"runStatusCounts.done = {done}, want {expected_runs} (durable rows did not all converge)")
    extra = {k: v for k, v in run_status_counts.items() if k != "done"}
    if extra:
        failures.append(f"runStatusCounts has non-done states: {extra}")
if peak is not None:
    if peak / 1e6 > ceiling_mb:
        failures.append(f"cgroup peak {peak / 1e6:.1f} MB exceeds the {ceiling_mb:.0f} MB GA ceiling")
    if peak / 1e6 > hard_limit_mb:
        failures.append(f"cgroup peak {peak / 1e6:.1f} MB exceeds the {hard_limit_mb:.0f} MB hard limit")
    if mem_max is not None and peak > mem_max:
        failures.append(f"cgroup peak {peak} exceeds memory.max {mem_max}")

summary = dict(bench)
summary.update({
    "mode": "g5-perf",
    "peakMB": round(peak / 1e6, 1) if peak is not None else None,
    "peakBytes": peak,
    "peakPctOfLimit": round(peak / 2147483648 * 100, 2) if peak is not None else None,
    "cgroupMaxBytes": mem_max,
    "ceilingMB": ceiling_mb,
    "hardLimitMB": hard_limit_mb,
    "expectedSessions": expected_sessions,
    "expectedRuns": expected_runs,
    "streamsActiveMax": streams_max,
    "runsActiveMax": runs_active_max,
    "hardSSEErrors": hard,
    "statusCounts429": throttled,
    "verdict": "FAIL" if failures else "PASS",
    "failures": failures,
})
with open(out_path, "w") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
    fh.write("\n")

print(json.dumps({k: summary.get(k) for k in (
    "mode", "sessions", "sseSessions", "streamsActiveMax", "texts", "durationSec",
    "runsActiveMax", "runsStartedFinal", "runsFinishedDone", "runsFailedFinal",
    "runsPendingFinal", "runStatusCounts", "imagesOK", "imagesErr",
    "sseErrors", "statusCounts429", "hardSSEErrors", "throughputMBs",
    "peakMB", "peakPctOfLimit", "ceilingMB", "verdict",
)}, ensure_ascii=False, indent=2))
for failure in failures:
    print(f"G5 PERF FAIL: {failure}")
print(f"result JSON: {out_path}")
if failures:
    sys.exit(1)
print(f"G5 PERF PASS: {expected_sessions} sessions, {expected_runs} concurrent runs, peak {peak/1e6:.0f} MB / 2048 MB")
PY

say "== perf profile done (cleanup follows) =="
