#!/usr/bin/env bash
# SIGTERM shutdown-drain smoke test (drain window observable):
#   hold one SSE stream open -> SIGTERM -> new writes get 503, stream gets the
#   drain frame -> process exits -> restart resumes the run from the expired lease.
#
# Hardened after a dirty-port incident: the script refuses to start when the
# port is taken, verifies the /v1/health pid is the process it launched, bounds
# the SSE reader, and cleans up everything on exit.
set -uo pipefail
# Usage: scripts/acceptance-drain.sh  (env overrides: KEEL_DRAIN_PORT, KEEL_DRAIN_DB,
# KEEL_ADMIN_KEY, KEEL_DRAIN_BIN). Requires docker (fde2e-keel-pg) and a built
# runtime binary; the database is created and dropped by the script.
DB="${KEEL_DRAIN_DB:-keel_drain_smoke}"
DSN="${KEEL_DRAIN_DSN:-postgres://postgres:runtime@127.0.0.1:13741/$DB?sslmode=disable}"
BIN="${KEEL_DRAIN_BIN:-/tmp/keel-drain-runtime}"
PORT="${KEEL_DRAIN_PORT:-13790}"
BASE="http://127.0.0.1:$PORT"
TENANT='aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
SESSION='bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
LOG=/tmp/keel-drain-smoke.log
SSE=/tmp/keel-drain-sse.txt
ADMIN_KEY="${KEEL_ADMIN_KEY:-drain-smoke-key}"
PASS=0
FAIL=0
RT_PID=''
SSE_PID=''

say() { printf '%s\n' "$*"; }
ok()  { PASS=$((PASS+1)); say "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); say "FAIL: $*"; }

cleanup() {
  [ -n "$SSE_PID" ] && kill -9 "$SSE_PID" 2>/dev/null
  [ -n "$RT_PID" ] && kill -9 "$RT_PID" 2>/dev/null
  docker exec fde2e-keel-pg psql -U postgres -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1
}
trap cleanup EXIT

# Interlock: the script creates and drops this database. Refuse names that do
# not look dedicated so a mistyped environment variable cannot wipe a shared
# database (the same guard cmd/r1diff and cmd/e2e use).
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *e2e*|*test*|*smoke*|*perf*|*accept*) ;;
  *) echo "FATAL: refusing to use database \"$DB\": the name must look dedicated (e2e/test/smoke/perf/accept)"; exit 1 ;;
esac

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  say "FATAL: port $PORT is already in use — refusing to run against a foreign process"
  open_pid=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}')
  say "holder pid: $open_pid ($(ps -o command= -p "$open_pid" 2>/dev/null | cut -c1-80))"
  exit 1
fi

docker exec fde2e-keel-pg psql -U postgres -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1
docker exec fde2e-keel-pg psql -U postgres -c "CREATE DATABASE $DB" >/dev/null

start_runtime() {
  KEEL_DSN="$DSN" KEEL_FAKE_DELAY_MS=4000 KEEL_ADMIN_KEY="$ADMIN_KEY" \
    "$BIN" -addr "127.0.0.1:$PORT" -cas /tmp/keel-drain-cas -streams 16 -runs 4 \
    -drain-window 6s -drain-close-rate 1 >> "$LOG" 2>&1 &
  RT_PID=$!
  for _ in $(seq 1 150); do
    health=$(curl -fsS -m 2 "$BASE/v1/health" 2>/dev/null || true)
    if [ -n "$health" ]; then
      # The process answering must be the one we just started: a stale runtime
      # on this port would make every later assertion meaningless.
      answered=$(printf '%s' "$health" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid"))' 2>/dev/null)
      if [ "$answered" = "$RT_PID" ]; then return 0; fi
      say "FATAL: port $PORT answers with pid $answered, not $RT_PID"
      exit 1
    fi
    if ! kill -0 "$RT_PID" 2>/dev/null; then say 'FATAL: runtime exited during startup'; exit 1; fi
    sleep 0.1
  done
  say 'FATAL: runtime did not become healthy'
  exit 1
}

say '== start #1 =='
start_runtime
curl -fsS -m 5 -X POST "$BASE/v1/tenants" -H "X-Tenant-ID: $TENANT" -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' -d '{"name":"drain-smoke"}' >/dev/null || { say 'FATAL: tenant create'; exit 1; }
curl -fsS -m 5 -X POST "$BASE/v1/sessions" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"$SESSION\"}" >/dev/null || { say 'FATAL: session create'; exit 1; }
curl -fsS -m 5 -X POST "$BASE/v1/sessions/$SESSION/input" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' \
  -d '{"text":"hello drain","requestId":"drain-1"}' >/dev/null || { say 'FATAL: input'; exit 1; }

# Hold one SSE stream open so the drain has something to drain. --max-time is
# the escape hatch: the reader can never hang the script.
: > "$SSE"
curl -sS -N --max-time 30 "$BASE/v1/sessions/$SESSION/events/stream" -H "X-Tenant-ID: $TENANT" >> "$SSE" 2>&1 &
SSE_PID=$!
sleep 1.5

say '== SIGTERM (run in flight, one stream open) =='
kill -TERM "$RT_PID"
sleep 0.4

if curl -fsS -m 2 "$BASE/v1/health" >/dev/null 2>&1; then ok 'health stays reachable during drain (probe exempt)'; else bad 'health unreachable during drain'; fi

CODE=$(curl -s -m 3 -o /tmp/keel-drain-late.json -w '%{http_code}' -X POST "$BASE/v1/sessions/$SESSION/input" \
  -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d '{"text":"late write","requestId":"drain-2"}')
say "late write during drain: HTTP $CODE body=$(cat /tmp/keel-drain-late.json 2>/dev/null)"
case "$CODE" in
  503) ok 'drain rejects new writes with 503' ;;
  *) bad "expected 503 during shutdown drain, got $CODE" ;;
esac

for _ in $(seq 1 80); do
  grep -q 'event: drain' "$SSE" 2>/dev/null && break
  sleep 0.1
done
if grep -q 'event: drain' "$SSE"; then ok 'SSE stream received the drain frame'; else bad 'no drain frame on the SSE stream'; fi
kill -9 "$SSE_PID" 2>/dev/null
wait "$SSE_PID" 2>/dev/null
SSE_PID=''

for _ in $(seq 1 150); do kill -0 "$RT_PID" 2>/dev/null || break; sleep 0.1; done
if kill -0 "$RT_PID" 2>/dev/null; then bad 'process still alive after drain window'; kill -9 "$RT_PID"; else ok 'process exited after drain'; fi
wait "$RT_PID" 2>/dev/null
RT_PID=''

say '== start #2 (resume from expired lease) =='
start_runtime
BODY=''
ASSIST=0
for _ in $(seq 1 240); do
  BODY=$(curl -fsS -m 3 "$BASE/v1/sessions/$SESSION/events?limit=50" -H "X-Tenant-ID: $TENANT" 2>/dev/null || true)
  ASSIST=$(printf '%s' "$BODY" | python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: print(0); raise SystemExit
print(sum(1 for e in d.get("events",[]) if e.get("kind")=="assistant.message"))' 2>/dev/null || echo 0)
  [ "$ASSIST" = "1" ] && break
  sleep 0.25
done
if [ "$ASSIST" = "1" ]; then ok 'run resumed after restart with exactly one assistant reply'; else bad "reply count after restart = $ASSIST"; fi
say "final events: $BODY"

kill -TERM "$RT_PID" 2>/dev/null
for _ in $(seq 1 100); do kill -0 "$RT_PID" 2>/dev/null || break; sleep 0.1; done
kill -9 "$RT_PID" 2>/dev/null
RT_PID=''
say "== cleanup done: $PASS pass / $FAIL fail =="
exit "$((FAIL > 0 ? 1 : 0))"
