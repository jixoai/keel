#!/usr/bin/env bash
# In-flight input probe: post user1, wait until the run is claimed/running, then
# post "please echo this". The runtime must answer BOTH: the echo tool must run
# exactly once and the final reply must quote its result.
#
# Fail-closed: a failed DROP/CREATE, a non-fresh database, a non-empty new
# session before the first input, a foreign process on the port, an unreadable
# event log, a duplicated requestId/tool result, or a changed log inside the
# stable window all exit non-zero.
set -euo pipefail
# Usage: scripts/acceptance-inflight.sh  (env: KEEL_INFLIGHT_PORT, KEEL_INFLIGHT_DB,
# KEEL_INFLIGHT_DSN, KEEL_ADMIN_KEY, KEEL_INFLIGHT_BIN,
# KEEL_INFLIGHT_STABLE_WINDOW_SEC). Requires docker and a dockerized Postgres
# named $KEEL_PG_CTR (override with KEEL_PG_CTR; default keel-pg) and a built
# runtime; the database is created and dropped by the script.
DB="${KEEL_INFLIGHT_DB:-keel_inflight_e2e}"; PORT="${KEEL_INFLIGHT_PORT:-13791}"
BASE="http://127.0.0.1:$PORT"
TENANT='cccccccc-cccc-4ccc-8ccc-cccccccccccc'
SESSION='dddddddd-dddd-4ddd-8ddd-dddddddddddd'
ADMIN_KEY="${KEEL_ADMIN_KEY:-inflight-probe-key}"
DSN="${KEEL_INFLIGHT_DSN:-postgres://postgres:runtime@127.0.0.1:13741/$DB?sslmode=disable}"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
STABLE_WINDOW_SEC="${KEEL_INFLIGHT_STABLE_WINDOW_SEC:-3}"
LOG=/tmp/keel-inflight.log
RT=''
PASS=0
FAIL=0

say() { printf '%s\n' "$*"; }
ok()  { PASS=$((PASS+1)); say "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); say "FAIL: $*"; }

case "$STABLE_WINDOW_SEC" in
  ''|*[!0-9]*) echo "FATAL: KEEL_INFLIGHT_STABLE_WINDOW_SEC must be a non-negative integer (got \"$STABLE_WINDOW_SEC\")"; exit 1 ;;
esac

cleanup() {
  local code=$?
  if [ -n "$RT" ]; then
    kill -9 "$RT" 2>/dev/null || true
    wait "$RT" 2>/dev/null || true
  fi
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT

case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *e2e*|*test*|*smoke*|*perf*|*accept*) ;;
  *) echo "FATAL: refusing to use database \"$DB\": the name must look dedicated"; exit 1 ;;
esac

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  say "FATAL: port $PORT is already in use — refusing to run against a foreign process"
  open_pid=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}')
  say "holder pid: $open_pid ($(ps -o command= -p "$open_pid" 2>/dev/null | cut -c1-80))"
  exit 1
fi

# A failed DROP/CREATE must stop the run: continuing against the previous
# database makes each assertion below meaningless.
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

rm -f "$LOG"
KEEL_DSN="$DSN" KEEL_FAKE_DELAY_MS=1500 KEEL_ADMIN_KEY="$ADMIN_KEY" \
  "${KEEL_INFLIGHT_BIN:-/tmp/keel-inflight-runtime}" -addr "127.0.0.1:$PORT" -cas /tmp/keel-inflight-cas -streams 16 -runs 4 >> "$LOG" 2>&1 &
RT=$!

healthy=0
for _ in $(seq 1 100); do
  health=$(curl -fsS -m 2 "$BASE/v1/health" 2>/dev/null || true)
  if [ -n "$health" ]; then
    answered=$(printf '%s' "$health" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid"))' 2>/dev/null || true)
    if [ "$answered" = "$RT" ]; then healthy=1; break; fi
    say "FATAL: port $PORT answers with pid $answered, not $RT"
    exit 1
  fi
  if ! kill -0 "$RT" 2>/dev/null; then say 'FATAL: runtime exited during startup'; exit 1; fi
  sleep 0.2
done
if [ "$healthy" != "1" ]; then say 'FATAL: runtime did not become healthy'; exit 1; fi

curl -fsS -X POST "$BASE/v1/tenants" -H "X-Tenant-ID: $TENANT" -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' -d '{"name":"inflight"}' >/dev/null
curl -fsS -X POST "$BASE/v1/sessions" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d "{\"sessionId\":\"$SESSION\"}" >/dev/null

# Freshness gate: the brand-new session must have zero events before the first
# input. Anything else means the database (or the session) was reused.
pre_events=$(curl -fsS -m 3 "$BASE/v1/sessions/$SESSION/events?limit=50" -H "X-Tenant-ID: $TENANT" 2>/dev/null || true)
pre_count=$(printf '%s' "$pre_events" | python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: print("unreadable"); raise SystemExit
print(len(d.get("events",[])))' 2>/dev/null || true)
if [ "$pre_count" != "0" ]; then
  say "FATAL: new session already has $pre_count event(s) before the first input — database was not reset"
  say "pre-input events: $pre_events"
  exit 1
fi
ok 'new session starts with zero events'

curl -fsS -X POST "$BASE/v1/sessions/$SESSION/input" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d '{"text":"hello inflight","requestId":"i-1"}' >/dev/null

# wait until the run is actually running (the first attempt has claimed)
st=''
for _ in $(seq 1 100); do
  st=$(curl -fsS "$BASE/v1/sessions/$SESSION/run" -H "X-Tenant-ID: $TENANT" 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status"))' 2>/dev/null || true)
  if [ "$st" = "running" ]; then break; fi
  sleep 0.05
done
say "run status before second input: ${st:-unknown}"

curl -fsS -X POST "$BASE/v1/sessions/$SESSION/input" -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' -d '{"text":"please echo this","requestId":"i-2"}' >/dev/null

# analyze_events <label> <json> [previous-summary-json]: prints a machine
# readable one-line summary and exits non-zero on any invariant violation. When
# a previous summary is supplied it also asserts the counts did not drift.
analyze_events() {
  local label=$1 body=$2 prev=${3:-}
  EVENTS_JSON="$body" PREV_JSON="$prev" python3 - "$label" <<'PY'
import json, os, sys
label = sys.argv[1]
try:
    events = json.loads(os.environ["EVENTS_JSON"]).get("events", [])
except Exception as exc:
    print(f"{label}: VIOLATION: events JSON unreadable: {exc}")
    sys.exit(1)
seqs = [e.get("seq") for e in events]
tools = [e for e in events if e.get("kind") == "tool.result"]
assist = [e for e in events if e.get("kind") == "assistant.message"]
failed = [e for e in events if e.get("kind") == "run.failed"]
users = [e for e in events if e.get("kind") == "user.message"]
request_ids = [(e.get("payload") or {}).get("requestId") for e in users]
texts = [c.get("text", "") for m in assist
         for c in ((m.get("payload") or {}).get("content") or []) if c.get("type") == "text"]
final_echo = [t for t in texts if "tool echo returned" in t]
problems = []
if seqs != list(range(1, len(seqs) + 1)):
    problems.append(f"event seq is not contiguous from 1: {seqs}")
if len(tools) != 1:
    problems.append(f"tool.result count = {len(tools)}, want exactly 1")
if request_ids.count("i-1") != 1:
    problems.append(f'user.message requestId "i-1" occurrences = {request_ids.count("i-1")}, want 1')
if request_ids.count("i-2") != 1:
    problems.append(f'user.message requestId "i-2" occurrences = {request_ids.count("i-2")}, want 1')
if len(request_ids) != len(set(request_ids)):
    problems.append(f"duplicate requestIds in the event log: {request_ids}")
if len(final_echo) != 1:
    problems.append(f"assistant texts quoting the tool result = {len(final_echo)}, want exactly 1")
if failed:
    problems.append(f"run.failed events = {len(failed)}")
summary = {
    "label": label,
    "events": len(events),
    "toolResults": len(tools),
    "assistantMessages": len(assist),
    "finalEchoTexts": len(final_echo),
    "requestIds": request_ids,
}
prev_raw = os.environ.get("PREV_JSON", "").strip()
if prev_raw:
    try:
        prev = json.loads(prev_raw)
    except Exception as exc:
        problems.append(f"previous summary unreadable: {exc}")
    else:
        for key in ("events", "toolResults", "assistantMessages", "finalEchoTexts"):
            if prev.get(key) != summary.get(key):
                problems.append(f"{key} drifted over the stable window: {prev.get(key)} -> {summary.get(key)}")
summary["problems"] = problems
print(json.dumps(summary, ensure_ascii=False))
for problem in problems:
    # stderr keeps the captured summary a single clean JSON line.
    print(f"{label}: VIOLATION: {problem}", file=sys.stderr)
sys.exit(1 if problems else 0)
PY
}

body=''
st=''
echo_count=0
# A run status of done can be observed at the end of the tool-call turn, before
# the follow-up turn that quotes the tool result. Wait for the final reply
# itself, not just the first done: otherwise the stable window starts too early
# and "sees" a legitimate late event as drift.
for _ in $(seq 1 300); do
  body=$(curl -fsS "$BASE/v1/sessions/$SESSION/events?limit=50" -H "X-Tenant-ID: $TENANT" 2>/dev/null || true)
  st=$(curl -fsS "$BASE/v1/sessions/$SESSION/run" -H "X-Tenant-ID: $TENANT" 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status"))' 2>/dev/null || true)
  echo_count=$(printf '%s' "$body" | python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: print(0); raise SystemExit
n=0
for e in d.get("events",[]):
    if e.get("kind")!="assistant.message": continue
    for c in ((e.get("payload") or {}).get("content") or []):
        if c.get("type")=="text" and "tool echo returned" in (c.get("text") or ""): n+=1
print(n)' 2>/dev/null || echo 0)
  case "$echo_count" in ''|*[!0-9]*) echo_count=0 ;; esac
  if [ "$st" = "done" ] && [ "$echo_count" -ge 1 ] 2>/dev/null; then break; fi
  sleep 0.2
done

if [ "$st" = "done" ]; then ok 'run reached done after the in-flight input'; else bad "run status = ${st:-unknown} after the in-flight input"; fi

if summary=$(analyze_events 't0' "$body"); then
  ok 'event log at t0: echo ran exactly once, both requestIds once, contiguous seq'
else
  bad 'event log invariants failed at t0'
fi
say "$summary"

say "== stable window: ${STABLE_WINDOW_SEC}s after done =="
sleep "$STABLE_WINDOW_SEC"
body=$(curl -fsS "$BASE/v1/sessions/$SESSION/events?limit=50" -H "X-Tenant-ID: $TENANT" 2>/dev/null || true)
if summary2=$(analyze_events "t+${STABLE_WINDOW_SEC}s" "$body" "$summary"); then
  ok "event log stayed stable over the ${STABLE_WINDOW_SEC}s window (no duplicate reply/tool run)"
else
  bad "event log changed/failed over the ${STABLE_WINDOW_SEC}s window"
fi
say "$summary2"

say "== cleanup done: $PASS pass / $FAIL fail =="
exit "$((FAIL > 0 ? 1 : 0))"
