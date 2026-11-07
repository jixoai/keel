#!/usr/bin/env bash
# SIGTERM shutdown-drain smoke test (drain window observable):
#   hold one SSE stream open -> SIGTERM -> new writes get 503, stream gets the
#   drain frame -> process exits -> restart resumes the run from the expired lease.
#
# Hardened after a dirty-port incident and a dirty-database false PASS: the
# script refuses to start when the port is taken, drops/creates with FORCE and
# fails when that fails, proves the database and the new session start empty,
# verifies the /v1/health pid is the process it launched, bounds the SSE reader,
# asserts a stable event log (exactly one final answer, contiguous seq, each
# requestId once) over a fixed window after the run is done, and cleans up
# everything on exit.
set -euo pipefail
# Usage: scripts/acceptance-drain.sh  (env overrides: KEEL_DRAIN_PORT, KEEL_DRAIN_DB,
# KEEL_ADMIN_KEY, KEEL_DRAIN_BIN, KEEL_DRAIN_STABLE_WINDOW_SEC). Requires docker
# and a dockerized Postgres named $KEEL_PG_CTR (override with KEEL_PG_CTR;
# default keel-pg), plus a built runtime binary; the database is created and
# dropped by the script.
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
STABLE_WINDOW_SEC="${KEEL_DRAIN_STABLE_WINDOW_SEC:-3}"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
PASS=0
FAIL=0
RT_PID=''
SSE_PID=''

say() { printf '%s\n' "$*"; }
ok()  { PASS=$((PASS+1)); say "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); say "FAIL: $*"; }

case "$STABLE_WINDOW_SEC" in
  ''|*[!0-9]*) echo "FATAL: KEEL_DRAIN_STABLE_WINDOW_SEC must be a non-negative integer (got \"$STABLE_WINDOW_SEC\")"; exit 1 ;;
esac

cleanup() {
  local code=$?
  if [ -n "$SSE_PID" ]; then
    kill -9 "$SSE_PID" 2>/dev/null || true
    wait "$SSE_PID" 2>/dev/null || true
  fi
  if [ -n "$RT_PID" ]; then
    kill -9 "$RT_PID" 2>/dev/null || true
    wait "$RT_PID" 2>/dev/null || true
  fi
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
  exit "$code"
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

# A failed DROP/CREATE must stop the run: continuing against the previous
# database is exactly how a stale assistant.message used to fake a PASS.
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
      answered=$(printf '%s' "$health" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid"))' 2>/dev/null || true)
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

# fetch_events prints the raw events JSON (empty when the request fails).
fetch_events() {
  curl -fsS -m 3 "$BASE/v1/sessions/$SESSION/events?limit=50" -H "X-Tenant-ID: $TENANT" 2>/dev/null || true
}

count_events() {
  printf '%s' "$1" | python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: print("unreadable"); raise SystemExit
print(len(d.get("events",[])))' 2>/dev/null || true
}

# check_event_log <label> <json>: invariant gate for one snapshot of the log.
# Fails when the log is not exactly: one assistant.message, one user.message
# carrying requestId drain-1, a contiguous seq range starting at 1, and no
# duplicated requestIds (the late write drain-2 must never be committed).
check_event_log() {
  local label=$1 body=$2
  # The body travels through the environment: stdin is taken by the heredoc.
  EVENTS_JSON="$body" python3 - "$label" <<'PY'
import json, os, sys
label = sys.argv[1]
try:
    events = json.loads(os.environ["EVENTS_JSON"]).get("events", [])
except Exception as exc:
    print(f"{label}: VIOLATION: events JSON unreadable: {exc}")
    sys.exit(1)
seqs = [e.get("seq") for e in events]
assist = [e for e in events if e.get("kind") == "assistant.message"]
users = [e for e in events if e.get("kind") == "user.message"]
request_ids = [(e.get("payload") or {}).get("requestId") for e in users]
problems = []
if len(assist) != 1:
    problems.append(f"assistant.message count = {len(assist)}, want exactly 1")
if seqs != list(range(1, len(seqs) + 1)):
    problems.append(f"event seq is not contiguous from 1: {seqs}")
if request_ids.count("drain-1") != 1:
    problems.append(f'user.message requestId "drain-1" occurrences = {request_ids.count("drain-1")}, want 1')
if len(request_ids) != len(set(request_ids)):
    problems.append(f"duplicate requestIds in the event log: {request_ids}")
if "drain-2" in request_ids:
    problems.append('late write requestId "drain-2" was committed during the drain')
print(f"{label}: events={len(events)} assistant={len(assist)} requestIds={request_ids} seqStart={seqs[:1]}")
for problem in problems:
    print(f"{label}: VIOLATION: {problem}")
sys.exit(1 if problems else 0)
PY
}

say '== start #1 =='
start_runtime
curl -fsS -m 5 -X POST "$BASE/v1/tenants" -H "X-Tenant-ID: $TENANT" -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' -d '{"name":"drain-smoke"}' >/dev/null || { say 'FATAL: tenant create'; exit 1; }
curl -fsS -m 5 -X POST "$BASE/v1/sessions" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"$SESSION\"}" >/dev/null || { say 'FATAL: session create'; exit 1; }

# Freshness gate: the brand-new session must have zero events before the first
# input. Anything else means the database (or the session) was reused.
PRE_EVENTS=$(fetch_events)
PRE_COUNT=$(count_events "$PRE_EVENTS")
if [ "$PRE_COUNT" != "0" ]; then
  say "FATAL: new session already has $PRE_COUNT event(s) before the first input — database was not reset"
  say "pre-input events: $PRE_EVENTS"
  exit 1
fi
ok 'new session starts with zero events'

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
say "late write during drain: HTTP $CODE body=$(cat /tmp/keel-drain-late.json 2>/dev/null || true)"
case "$CODE" in
  503) ok 'drain rejects new writes with 503' ;;
  *) bad "expected 503 during shutdown drain, got $CODE" ;;
esac

for _ in $(seq 1 80); do
  if grep -q 'event: drain' "$SSE" 2>/dev/null; then break; fi
  sleep 0.1
done
if grep -q 'event: drain' "$SSE"; then ok 'SSE stream received the drain frame'; else bad 'no drain frame on the SSE stream'; fi
kill -9 "$SSE_PID" 2>/dev/null || true
wait "$SSE_PID" 2>/dev/null || true
SSE_PID=''

for _ in $(seq 1 150); do kill -0 "$RT_PID" 2>/dev/null || break; sleep 0.1; done
if kill -0 "$RT_PID" 2>/dev/null; then bad 'process still alive after drain window'; kill -9 "$RT_PID" 2>/dev/null || true; else ok 'process exited after drain'; fi
wait "$RT_PID" 2>/dev/null || true
RT_PID=''

say '== start #2 (resume from expired lease) =='
start_runtime
BODY=''
STATUS=''
ASSIST=0
# Wait for the durable run state, not just a first glimpse of a reply: the old
# loop broke on the first assistant.message and was blind to a duplicate final
# answer written afterwards.
for _ in $(seq 1 240); do
  BODY=$(fetch_events)
  ASSIST=$(printf '%s' "$BODY" | python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: print(0); raise SystemExit
print(sum(1 for e in d.get("events",[]) if e.get("kind")=="assistant.message"))' 2>/dev/null || echo 0)
  case "$ASSIST" in ''|*[!0-9]*) ASSIST=0 ;; esac
  STATUS=$(curl -fsS -m 3 "$BASE/v1/sessions/$SESSION/run" -H "X-Tenant-ID: $TENANT" 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status"))' 2>/dev/null || true)
  if [ "$STATUS" = "done" ] && [ "$ASSIST" -ge 1 ] 2>/dev/null; then break; fi
  sleep 0.25
done

if [ "$STATUS" = "done" ]; then ok 'run reached done after restart'; else bad "run status after restart = ${STATUS:-unknown}"; fi
BODY=$(fetch_events)
if check_event_log 't0' "$BODY"; then ok 'event log at t0 has exactly one assistant reply, contiguous seq, requestId drain-1 once'; else bad 'event log invariants failed at t0'; fi

say "== stable window: ${STABLE_WINDOW_SEC}s after done =="
sleep "$STABLE_WINDOW_SEC"
BODY=$(fetch_events)
if check_event_log "t+${STABLE_WINDOW_SEC}s" "$BODY"; then ok "event log stayed stable over the ${STABLE_WINDOW_SEC}s window"; else bad "event log changed/failed over the ${STABLE_WINDOW_SEC}s window"; fi
say "final events: $BODY"

kill -TERM "$RT_PID" 2>/dev/null || true
for _ in $(seq 1 100); do kill -0 "$RT_PID" 2>/dev/null || break; sleep 0.1; done
kill -9 "$RT_PID" 2>/dev/null || true
RT_PID=''
say "== cleanup done: $PASS pass / $FAIL fail =="
exit "$((FAIL > 0 ? 1 : 0))"
