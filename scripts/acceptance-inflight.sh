#!/usr/bin/env bash
# In-flight input probe: post user1, wait until the run is claimed/running, then
# post "please echo this". The runtime must answer BOTH: the echo tool must run
# exactly once and the final reply must quote its result.
set -uo pipefail
# Usage: scripts/acceptance-inflight.sh  (env: KEEL_INFLIGHT_PORT, KEEL_INFLIGHT_DB,
# KEEL_ADMIN_KEY, KEEL_INFLIGHT_BIN). Requires docker (fde2e-keel-pg) and a
# built runtime; the database is created and dropped by the script.
DB="${KEEL_INFLIGHT_DB:-keel_inflight_e2e}"; PORT="${KEEL_INFLIGHT_PORT:-13791}"
BASE="http://127.0.0.1:$PORT"
TENANT='cccccccc-cccc-4ccc-8ccc-cccccccccccc'
SESSION='dddddddd-dddd-4ddd-8ddd-dddddddddddd'
ADMIN_KEY="${KEEL_ADMIN_KEY:-inflight-probe-key}"
DSN="postgres://postgres:runtime@127.0.0.1:13741/$DB?sslmode=disable"
LOG=/tmp/keel-inflight.log
rm -f "$LOG"
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *e2e*|*test*|*smoke*|*perf*|*accept*) ;;
  *) echo "FATAL: refusing to use database \"$DB\": the name must look dedicated"; exit 1 ;;
esac
docker exec fde2e-keel-pg psql -U postgres -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1
docker exec fde2e-keel-pg psql -U postgres -c "CREATE DATABASE $DB" >/dev/null
KEEL_DSN="$DSN" KEEL_FAKE_DELAY_MS=1500 KEEL_ADMIN_KEY="$ADMIN_KEY" \
  "${KEEL_INFLIGHT_BIN:-/tmp/keel-inflight-runtime}" -addr "127.0.0.1:$PORT" -cas /tmp/keel-inflight-cas -streams 16 -runs 4 >> "$LOG" 2>&1 &
RT=$!
trap 'kill -9 $RT 2>/dev/null; docker exec fde2e-keel-pg psql -U postgres -c "DROP DATABASE IF EXISTS '"$DB"'" >/dev/null 2>&1' EXIT
for _ in $(seq 1 100); do curl -fsS -m 2 "$BASE/v1/health" >/dev/null 2>&1 && break; sleep 0.2; done
curl -fsS -X POST "$BASE/v1/tenants" -H "X-Tenant-ID: $TENANT" -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' -d '{"name":"inflight"}' >/dev/null
curl -fsS -X POST "$BASE/v1/sessions" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d "{\"sessionId\":\"$SESSION\"}" >/dev/null
curl -fsS -X POST "$BASE/v1/sessions/$SESSION/input" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d '{"text":"hello inflight","requestId":"i-1"}' >/dev/null
# wait until the run is actually running (the first attempt has claimed)
for _ in $(seq 1 100); do
  st=$(curl -fsS "$BASE/v1/sessions/$SESSION/run" -H "X-Tenant-ID: $TENANT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status"))' 2>/dev/null)
  [ "$st" = "running" ] && break
  sleep 0.05
done
echo "run status before second input: ${st:-unknown}"
curl -fsS -X POST "$BASE/v1/sessions/$SESSION/input" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d '{"text":"please echo this","requestId":"i-2"}' >/dev/null
for _ in $(seq 1 300); do
  body=$(curl -fsS "$BASE/v1/sessions/$SESSION/events?limit=50" -H "X-Tenant-ID: $TENANT")
  done_count=$(printf '%s' "$body" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(1 for e in d["events"] if e["kind"]=="tool.result"))' 2>/dev/null || echo 0)
  st=$(curl -fsS "$BASE/v1/sessions/$SESSION/run" -H "X-Tenant-ID: $TENANT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status"))' 2>/dev/null)
  [ "$done_count" = "1" ] && [ "$st" = "done" ] && break
  sleep 0.2
done
printf '%s' "$body" | python3 -c "
import json,sys
d=json.load(sys.stdin)['events']
kinds=[e['kind'] for e in d]
tools=[e for e in d if e['kind']=='tool.result']
final=[e for e in d if e['kind']=='assistant.message']
texts=[c.get('text','') for m in final for c in (m['payload'].get('content') or []) if c.get('type')=='text']
ok = len(tools)==1 and any('tool echo returned' in t for t in texts)
print('events:', kinds)
print('tool.result count:', len(tools), '| final texts:', texts)
print('verdict:', 'PASS (in-flight input answered, echo ran exactly once)' if ok else 'FAIL')
sys.exit(0 if ok else 1)
"
