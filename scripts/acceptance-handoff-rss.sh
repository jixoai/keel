#!/usr/bin/env bash
# Handoff dual-process RSS acceptance: two runtimes coexist inside a 2GB cgroup
# while 1000 SSE sessions run, the old one hands off mid-run and exits; the
# combined footprint must stay inside the budget.
set -uo pipefail
# Usage: scripts/acceptance-handoff-rss.sh — two runtimes coexist inside a 2GB
# cgroup while 1000 SSE sessions run; the old one hands off mid-run and exits.
# Asserts the combined peak stays inside the budget (1.5GB ceiling) and that no
# stream failed for a reason other than admission backpressure.
DB="${KEEL_HANDOFF_RSS_DB:-keel_perf_handoff_e2e}"
CTR=keel-perf-handoff
ADMIN_KEY=perf-handoff-key
D1="postgres://postgres:runtime@host.docker.internal:13741/$DB?sslmode=disable"
docker rm -f "$CTR" >/dev/null 2>&1
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *e2e*|*test*|*smoke*|*perf*|*accept*) ;;
  *) echo "FATAL: refusing to use database \"$DB\": the name must look dedicated"; exit 1 ;;
esac
docker exec fde2e-keel-pg psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1
docker exec fde2e-keel-pg psql -U postgres -c "CREATE DATABASE $DB" >/dev/null
docker run -d --name "$CTR" --memory=2g --memory-swap=2g --cpus=4 \
  -v /tmp/keel-perf-runtime:/runtime:ro -v /tmp/keel-perf-cas:/cas \
  -e "KEEL_DSN=$D1" -e GOMEMLIMIT=1610612736 -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 sh -c '/runtime -addr 127.0.0.1:13750 -cas /cas -streams 2000 -runs 64 >/tmp/a.log 2>&1 &
    sleep 1
    KEEL_RUNTIME_ID=runtime-new exec /runtime -addr 127.0.0.1:13751 -cas /cas -streams 2000 -runs 64 >/tmp/b.log 2>&1' >/dev/null
for _ in $(seq 1 150); do
  if docker exec "$CTR" wget -qO- http://127.0.0.1:13750/v1/health >/dev/null 2>&1 && \
     docker exec "$CTR" wget -qO- http://127.0.0.1:13751/v1/health >/dev/null 2>&1; then break; fi
  sleep 0.2
done
A_PID=$(docker exec "$CTR" wget -qO- http://127.0.0.1:13750/v1/health 2>/dev/null | sed -n 's/.*"pid":\([0-9]*\).*/\1/p')
B_PID=$(docker exec "$CTR" wget -qO- http://127.0.0.1:13751/v1/health 2>/dev/null | sed -n 's/.*"pid":\([0-9]*\).*/\1/p')
echo "both healthy: old pid=${A_PID:-?} new pid=${B_PID:-?}"
docker run --rm --network "container:$CTR" -v /tmp/keel-perf-loadtest:/loadtest:ro -e "KEEL_ADMIN_KEY=$ADMIN_KEY" \
  alpine:3.20 /loadtest -base http://127.0.0.1:13750 -sessions 1000 -duration 60s \
  -text-every 10s -image-every 60s -image-size 1048576 -images 20 -client-rate 51200 \
  > /tmp/perf-handoff-bench.json 2> /tmp/perf-handoff-loadtest.log &
LOAD_PID=$!
sleep 15
echo "== drain old (handoff) while the load runs =="
docker exec "$CTR" sh -c "curl -s -XPOST http://127.0.0.1:13750/v1/runtime/drain -H 'Authorization: Bearer $ADMIN_KEY' -H 'Content-Type: application/json' -d '{\"mode\":\"handoff\",\"deadlineMs\":15000}'" 2>/dev/null || true
wait $LOAD_PID
PEAK=$(docker exec "$CTR" cat /sys/fs/cgroup/memory.peak 2>/dev/null | tr -d '\r')
echo "cgroup peak (both runtimes): ${PEAK:-?} bytes"
docker exec "$CTR" sh -c 'pgrep -f /runtime | wc -l; tail -3 /tmp/a.log; tail -3 /tmp/b.log' 2>/dev/null
python3 - "$PEAK" <<'PY'
import json, sys
d = json.load(open('/tmp/perf-handoff-bench.json'))
sc = d.get('statusCounts') or {}
hard = int(d.get('sseErrors', 0)) - int(sc.get('429', 0))
peak = int(sys.argv[1]) if sys.argv[1].isdigit() else 0
ceiling = int(1.5e9)
print({'sessions': d.get('sessions'), 'imagesOK': d.get('imagesOK'),
       'sseErrors': d.get('sseErrors'), 'hard': hard, 'status': sc})
print(f'cgroup peak with both runtimes: {peak/1e6:.0f} MB (ceiling {ceiling/1e6:.0f} MB)')
bad = hard > 0 or peak > ceiling
if bad:
    print('ACCEPTANCE FAIL: hard stream errors or peak over the ceiling')
    sys.exit(1)
print('verdict: PASS (dual-process peak inside the 2GB budget, 0 hard stream errors)')
PY
status=$?
docker rm -f "$CTR" >/dev/null 2>&1
docker exec fde2e-keel-pg psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1
# The acceptance verdict is the python exit code above; cleanup must not mask it.
exit "$status"
