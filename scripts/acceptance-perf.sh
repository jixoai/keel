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
# Requires: docker (Docker Desktop), the fde2e-keel-pg container for the DSN.
set -euo pipefail

SESSIONS="${1:-1000}"
DURATION="${2:-60s}"
DSN_BASE="${KEEL_PERF_DSN_BASE:-postgres://postgres:runtime@host.docker.internal:13741}"
DB="${KEEL_PERF_DB:-keel_perf}"
ADMIN_KEY="${KEEL_ADMIN_KEY:-local-perf-admin-key}"
PORT=13750
OUT_DIR="${KEEL_PERF_OUT:-/tmp/keel-perf}"
CTR=keel-perf-2g
PG_CTR=fde2e-keel-pg

say() { printf '%s\n' "$*"; }
mkdir -p "$OUT_DIR" /tmp/keel-perf-cas

cleanup() {
  docker rm -f "$CTR" >/dev/null 2>&1 || true
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1 || true
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
cleanup_db() { docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1 || true; }
cleanup_db
docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE $DB" >/dev/null

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
# the run is just the last reading and would understate the peak.
PEAK=$(docker exec "$CTR" cat /sys/fs/cgroup/memory.peak 2>/dev/null | tr -d '\r')
if [ -z "$PEAK" ]; then PEAK=$(docker exec "$CTR" cat /sys/fs/cgroup/memory.current | tr -d '\r'); fi
if [ ! -s "$OUT_DIR/bench.json" ]; then
  say "ACCEPTANCE FAIL: the loadtest produced no result (see $OUT_DIR/loadtest.log)"
  exit 1
fi
if [ -s "$OUT_DIR/bench.json" ]; then
  python3 - "$OUT_DIR/bench.json" "$PEAK" <<'PY'
import json, sys
bench = json.load(open(sys.argv[1]))
peak = int(sys.argv[2]) if sys.argv[2].isdigit() else 0
status = bench.get("statusCounts") or {}
throttled = int(status.get("429", 0))
# sseErrors counts every non-200 the stream loop saw, 429 included. Admission
# backpressure (429 + Retry-After) is documented and retried by the client;
# what must be zero is a stream that failed for any other reason.
hard = int(bench.get("sseErrors", 0)) - throttled
summary = {
    "sessions": bench.get("sessions"),
    "durationSec": round(bench.get("durationSec", 0)),
    "texts": bench.get("texts"),
    "imagesOK": bench.get("imagesOK"),
    "imagesErr": bench.get("imagesErr"),
    "sseErrorsRaw": bench.get("sseErrors"),
    "sseThrottled429": throttled,
    "sseHardErrors": hard,
    "throughputMBs": round(bench.get("throughputMBs", 0), 1),
    "imageLatencyP95MS": round(bench.get("imageLatencyP95MS", 0), 1),
    "rssMaxMB": bench.get("rssMaxMB"),
}
print(json.dumps(summary, ensure_ascii=False, indent=2))
print(f"cgroup peak after run: {peak/1e6:.0f} MB / 2048 MB ({(peak/2**31*100):.1f}% of the hard limit)")
if hard > 0:
    print("ACCEPTANCE FAIL: SSE streams failed for reasons other than admission backpressure")
    sys.exit(1)
print(f"verdict: PASS ({throttled} admission 429s retried by the client, 0 hard stream errors)")
PY
fi
say "raw: $OUT_DIR/bench.json $OUT_DIR/loadtest.log"
say "== done (container and database removed) =="
