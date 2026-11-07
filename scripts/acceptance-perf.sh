#!/usr/bin/env bash
# 2GB hard-limit acceptance: 1000 concurrent SSE sessions + scheduled image
# streams inside a 2 GB cgroup, against an isolated database.
#
# What it proves (the change's W6 memory/concurrency bar):
#   - the runtime survives the documented profile inside --memory=2g with no OOM
#   - the cgroup peak stays far below the limit
#   - SSE streams do not drop (0 errors) and the 429 backpressure behaves
#
# Usage: scripts/acceptance-perf.sh [sessions] [duration]
# Requires: docker (Docker Desktop) and a dockerized Postgres named
# $KEEL_PG_CTR (override with KEEL_PG_CTR; default keel-pg).
#
# Fail-closed by design: unreadable cgroup memory.peak, a peak above the
# ceiling, sessions != 1000, no text load, or any non-429 SSE error makes the
# script exit non-zero. The merged result (loadtest fields + acceptance fields)
# is written to $KEEL_PERF_OUT (default /tmp/keel-perf/acceptance.json).
set -euo pipefail

SESSIONS="${1:-1000}"
DURATION="${2:-60s}"
REQ_DURATION_SEC="${DURATION%s}"
DSN_BASE="${KEEL_PERF_DSN_BASE:-postgres://postgres:runtime@host.docker.internal:13741}"
DB="${KEEL_PERF_DB:-keel_perf}"
ADMIN_KEY="${KEEL_ADMIN_KEY:-local-perf-admin-key}"
PORT=13750
OUT_DIR="${KEEL_PERF_DIR:-/tmp/keel-perf}"
JSON_OUT="${KEEL_PERF_OUT:-$OUT_DIR/acceptance.json}"
CEILING_MB="${KEEL_PERF_CEILING_MB:-1536}"
EXPECTED_SESSIONS="${KEEL_PERF_EXPECTED_SESSIONS:-1000}"
CTR=keel-perf-2g
PG_CTR="${KEEL_PG_CTR:-keel-pg}"

say() { printf '%s\n' "$*"; }

case "$CEILING_MB" in
  ''|*[!0-9]*) echo "FATAL: KEEL_PERF_CEILING_MB must be a positive integer (got \"$CEILING_MB\")"; exit 1 ;;
esac
case "$EXPECTED_SESSIONS" in
  ''|*[!0-9]*) echo "FATAL: KEEL_PERF_EXPECTED_SESSIONS must be a positive integer (got \"$EXPECTED_SESSIONS\")"; exit 1 ;;
esac
[ "$CEILING_MB" -gt 0 ] || { echo "FATAL: KEEL_PERF_CEILING_MB must be > 0"; exit 1; }
[ "$EXPECTED_SESSIONS" -gt 0 ] || { echo "FATAL: KEEL_PERF_EXPECTED_SESSIONS must be > 0"; exit 1; }

mkdir -p "$OUT_DIR" /tmp/keel-perf-cas "$(dirname "$JSON_OUT")"

cleanup() {
  docker rm -f "$CTR" >/dev/null 2>&1 || true
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Interlock: the script creates and drops this database. Refuse names that do
# not look dedicated so a mistyped environment variable cannot wipe a shared
# database (the same guard cmd/r1diff and cmd/e2e use).
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *e2e*|*test*|*smoke*|*perf*|*accept*) ;;
  *) echo "FATAL: refusing to use database \"$DB\": the name must look dedicated (e2e/test/smoke/perf/accept)"; exit 1 ;;
esac

say "== build linux binaries =="
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/keel-perf-runtime ./cmd/runtime
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/keel-perf-loadtest ./cmd/loadtest

say "== fresh database $DB =="
cleanup_db() {
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null \
    || { say "FATAL: could not drop database $DB (is the $PG_CTR container running?)"; exit 1; }
}
cleanup_db
docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE $DB" >/dev/null \
  || { say "FATAL: could not create database $DB"; exit 1; }

say "== start runtime under --memory=2g =="
docker run -d --name "$CTR" --memory=2g --memory-swap=2g --cpus=4 \
  -v /tmp/keel-perf-runtime:/runtime:ro -v /tmp/keel-perf-cas:/cas \
  -e "KEEL_DSN=$DSN_BASE/$DB?sslmode=disable" \
  -e GOMEMLIMIT=1610612736 \
  -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 /runtime -addr 127.0.0.1:$PORT -cas /cas -streams 2000 >/dev/null

for _ in $(seq 1 150); do
  if docker exec "$CTR" wget -qO- "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
docker exec "$CTR" wget -qO- "http://127.0.0.1:$PORT/v1/health" >/dev/null || { say 'runtime did not become healthy'; docker logs "$CTR" | tail -20; exit 1; }
say "runtime healthy (pid $(docker exec "$CTR" pidof /runtime 2>/dev/null || echo '?'))"

say "== loadtest: $SESSIONS sessions / $DURATION (sidecar shares the netns) =="
docker run --rm --network "container:$CTR" \
  -v /tmp/keel-perf-loadtest:/loadtest:ro \
  -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 /loadtest -base "http://127.0.0.1:$PORT" \
  -sessions "$SESSIONS" -duration "$DURATION" \
  -text-every 10s -image-every 60s -image-size 1048576 -images 20 \
  -client-rate 51200 > "$OUT_DIR/bench.json" 2> "$OUT_DIR/loadtest.log" || true

# memory.peak is the cgroup's own high-water mark; memory.current sampled after
# the run is just the last reading and would understate the peak. There is no
# fallback: an unreadable peak is an uncertifiable run (fail closed).
PEAK=$(docker exec "$CTR" cat /sys/fs/cgroup/memory.peak 2>/dev/null | tr -d '\r' || true)
if [ ! -s "$OUT_DIR/bench.json" ]; then
  say "ACCEPTANCE FAIL: the loadtest produced no result (see $OUT_DIR/loadtest.log)"
  docker logs "$CTR" 2>&1 | tail -20 || true
  printf '{\n  "mode": "perf",\n  "verdict": "FAIL",\n  "failures": ["loadtest produced no result"]\n}\n' > "$JSON_OUT"
  say "result JSON: $JSON_OUT"
  exit 1
fi

python3 - "$OUT_DIR/bench.json" "$PEAK" "$JSON_OUT" "$CEILING_MB" "$EXPECTED_SESSIONS" "$REQ_DURATION_SEC" <<'PY'
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
# sseErrors counts every non-200 the stream loop saw, 429 included. Admission
# backpressure (429 + Retry-After) is documented and retried by the client;
# what must be zero is a stream that failed for any other reason.
hard = sse_errors - throttled

sessions = bench.get("sessions")
texts = bench.get("texts")
duration = bench.get("durationSec", 0)
images_ok = bench.get("imagesOK")

if sessions != expected_sessions:
    failures.append(f"sessions = {sessions}, want exactly {expected_sessions} (empty-run guard)")
if not isinstance(texts, int) or texts <= 0:
    failures.append(f"texts = {texts}, want > 0 (no text load means the run was idle)")
# A short run would pass every count above and still show a low peak: the
# duration and image channel must prove the budget was actually exercised.
if not isinstance(duration, (int, float)) or duration < 0.9 * expected_duration:
    failures.append(f"durationSec = {duration}, want >= {0.9 * expected_duration:.1f} of {expected_duration:.0f}s (short run)")
if not isinstance(images_ok, int) or images_ok <= 0:
    failures.append(f"imagesOK = {images_ok}, want > 0 (the image channel was never exercised)")
if hard != 0:
    failures.append(f"hard SSE errors = {hard} (sseErrors {sse_errors} - 429 {throttled}), want 0")
if peak is not None and peak / 1e6 > ceiling_mb:
    failures.append(f"cgroup peak {peak / 1e6:.0f} MB exceeds the {ceiling_mb:.0f} MB ceiling")

summary = dict(bench)  # keep every loadtest field
summary.update({
    "mode": "perf",
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
    "hardErrors", "imagesOK", "imagesErr", "throughputMBs", "peakMB", "peakBytes",
    "ceilingMB", "verdict",
)}, ensure_ascii=False, indent=2))
if peak is None:
    print("cgroup peak after run: UNREADABLE (failing closed)")
else:
    print(f"cgroup peak after run: {peak/1e6:.0f} MB / 2048 MB ({(peak/2**31*100):.1f}% of the hard limit)")
for failure in failures:
    print(f"ACCEPTANCE FAIL: {failure}")
print(f"result JSON: {out_path}")
if failures:
    sys.exit(1)
print(f"verdict: PASS ({throttled} admission 429s retried by the client, 0 hard stream errors)")
PY
say "raw: $OUT_DIR/bench.json $OUT_DIR/loadtest.log"
say "== done (container and database removed) =="
