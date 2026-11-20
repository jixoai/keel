#!/usr/bin/env bash
# acceptance-g5-chaos.sh — the G5 crash-consistency soak: random SIGKILL of the
# runtime every 30s for 10 minutes while clients keep posting input, then a
# strict ledger audit.
#
# cmd/chaos already owns the fault loop and the invariants; this wrapper gives
# it a dedicated database, dedicated ports, per-run log/CAS paths, and a
# fail-closed JSON verdict. The result records every kill timestamp so the
# report can prove the cadence instead of trusting the flag.
#
# Usage:
#   scripts/acceptance-g5-chaos.sh <evidence-dir>
#
# The DSN comes from KEEL_TEST_DSN or .env.local (never printed). The chaos
# database is <test-db>_chaos and is dropped on exit. Subscale dry runs require
# KEEL_G5_ALLOW_SUBSCALE=1; the formal gate requires 600s / 30s.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT_DIR="${1:-${KEEL_G5_CHAOS_OUT:-/tmp/keel-g5-chaos}}"
mkdir -p "$OUT_DIR"

SESSIONS="${KEEL_G5_CHAOS_SESSIONS:-16}"
DURATION="${KEEL_G5_CHAOS_DURATION:-600s}"
KILL_EVERY="${KEEL_G5_CHAOS_KILL_EVERY:-30s}"
SETTLE="${KEEL_G5_CHAOS_SETTLE:-20s}"
PORT="${KEEL_G5_CHAOS_PORT:-13760}"
ADMIN_KEY="${KEEL_G5_CHAOS_ADMIN_KEY:-g5-chaos-admin}"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
RUNTIME_BIN="/tmp/keel-g5-chaos-runtime"
CHAOS_BIN="/tmp/keel-g5-chaos"
CAS="/tmp/keel-g5-chaos-cas"

say() { printf '%s\n' "$*"; }
fatal() { say "G5 CHAOS FATAL: $*"; exit 1; }

DURATION_SEC="${DURATION%s}"
KILL_EVERY_SEC="${KILL_EVERY%s}"
SETTLE_SEC="${SETTLE%s}"
case "$DURATION_SEC" in ''|*[!0-9]*) fatal "duration must be seconds like 600s (got $DURATION)" ;; esac
case "$KILL_EVERY_SEC" in ''|*[!0-9]*) fatal "kill interval must be seconds like 30s (got $KILL_EVERY)" ;; esac
case "$SETTLE_SEC" in ''|*[!0-9]*) fatal "settle must be seconds like 20s (got $SETTLE)" ;; esac

EXPECTED_KILLS=$((DURATION_SEC / KILL_EVERY_SEC))
if [ "${KEEL_G5_ALLOW_SUBSCALE:-0}" != "1" ]; then
  [ "$DURATION_SEC" -eq 600 ] || fatal "formal chaos requires 600s (got $DURATION)"
  [ "$KILL_EVERY_SEC" -eq 30 ] || fatal "formal chaos requires a kill every 30s (got $KILL_EVERY)"
  EXPECTED_KILLS=20
fi

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
print(urllib.parse.urlparse(sys.argv[1]).path.lstrip('/') + '_chaos')
PY
)
CHAOS_DSN=$(python3 -I - "$KEEL_TEST_DSN" "$DB" <<'PY'
import sys, urllib.parse
u = urllib.parse.urlparse(sys.argv[1])
print(urllib.parse.urlunparse(u._replace(path='/' + sys.argv[2])))
PY
)
[ -n "$DB" ] || fatal "could not read a database name from the DSN"
case "$(printf %s "$DB" | tr '[:upper:]' '[:lower:]')" in
  *test*|*chaos*|*accept*|*e2e*|*smoke*) ;;
  *) fatal "refusing database \"$DB\": name must look dedicated (test/chaos/accept/e2e/smoke)" ;;
esac

docker exec "$PG_CTR" true >/dev/null 2>&1 || fatal "PostgreSQL container $PG_CTR is not running"
for p in "$PORT" "$((PORT + 1))"; do
  if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    fatal "port $p is already in use: $(lsof -nP -iTCP:"$p" -sTCP:LISTEN | tail -1)"
  fi
done

cleanup() {
  code=$?
  # The harness owns its children and stops them on every exit path; this is
  # the belt-and-braces sweep for a harness killed mid-cycle.
  ORPHANS=$(pgrep -f "$RUNTIME_BIN" 2>/dev/null || true)
  if [ -n "$ORPHANS" ]; then
    # shellcheck disable=SC2086
    kill -9 $ORPHANS >/dev/null 2>&1 || true
  fi
  {
    echo "exit=$code"
    echo "orphans_killed=${ORPHANS:-none}"
    if docker exec "$PG_CTR" psql -U postgres -q -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1; then
      echo "database_dropped=$DB"
    else
      echo "database_drop=FAILED:$DB"
    fi
    for p in "$PORT" "$((PORT + 1))"; do
      if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "port_$p=STILL_LISTENING"
      else
        echo "port_$p=free"
      fi
    done
    if pgrep -f "$RUNTIME_BIN" >/dev/null 2>&1; then
      echo "runtime_processes=STILL_PRESENT"
    else
      echo "runtime_processes=none"
    fi
  } >"$OUT_DIR/cleanup.txt"
  rm -rf "$CAS" 2>/dev/null || true
  rm -f "$RUNTIME_BIN" "$CHAOS_BIN" 2>/dev/null || true
  exit "$code"
}
trap cleanup EXIT

say "== G5 chaos: SIGKILL every $KILL_EVERY for $DURATION, $SESSIONS sessions =="
say "   database=$DB ports=$PORT/$((PORT + 1)) settle=$SETTLE"

say "-- build runtime and harness"
go build -o "$RUNTIME_BIN" ./cmd/runtime
go build -o "$CHAOS_BIN" ./cmd/chaos

say "-- fresh database $DB"
docker exec "$PG_CTR" psql -U postgres -q \
  -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" \
  -c "CREATE DATABASE \"$DB\"" >/dev/null \
  || fatal "could not reset database $DB"

say "-- run chaos (kill mode, faults unbounded within duration)"
set +e
# The DSN travels through KEEL_DSN (the harness's documented env fallback),
# never as an argv element: `ps` must not be able to read the credential.
KEEL_DSN="$CHAOS_DSN" "$CHAOS_BIN" -bin "$RUNTIME_BIN" -admin-key "$ADMIN_KEY" \
  -mode kill -faults 0 -port "$PORT" -sessions "$SESSIONS" \
  -duration "$DURATION" -kill-every "$KILL_EVERY" -settle "$SETTLE" \
  -log "$OUT_DIR/runtime.log" -cas "$CAS" -out "$OUT_DIR/chaos-kill.json" \
  >"$OUT_DIR/chaos.log" 2>&1
CHAOS_EXIT=$?
set -e
say "   harness exit=$CHAOS_EXIT"

say "-- assert chaos invariants"
python3 -I - "$OUT_DIR/chaos-kill.json" "$OUT_DIR/result.json" \
  "$EXPECTED_KILLS" "$KILL_EVERY_SEC" "$DURATION_SEC" "$SESSIONS" "$CHAOS_EXIT" <<'PY'
import json
import sys
from datetime import datetime

(json_path, out_path, expected_kills, kill_every, duration, sessions,
 chaos_exit) = sys.argv[1:8]
expected_kills = int(expected_kills)
kill_every = float(kill_every)
duration = float(duration)
sessions = int(sessions)
chaos_exit = int(chaos_exit)
failures = []

try:
    with open(json_path) as fh:
        raw = json.load(fh)
    if not isinstance(raw, dict):
        raise ValueError(f"expected an object, got {type(raw).__name__}")
except Exception as exc:
    raw = {}
    failures.append(f"chaos JSON unreadable ({json_path}): {exc}")

kills = raw.get("kills") or []
faults = int(raw.get("faults") or 0)
restarts = int(raw.get("restarts") or 0)
counters = {
    "seqGaps": int(raw.get("seqGaps") or 0),
    "duplicateRequestIDs": int(raw.get("duplicateRequestIDs") or 0),
    "duplicateFinalReplies": int(raw.get("duplicateFinalReplies") or 0),
    "unansweredNewest": int(raw.get("unansweredNewest") or 0),
    "orphanToolResults": int(raw.get("orphanToolResults") or 0),
    "unreadAfterSettle": int(raw.get("unreadAfterSettle") or 0),
    "stuckRuns": int(raw.get("stuckRuns") or 0),
    "runStatusErrors": int(raw.get("runStatusErrors") or 0),
    "eventLoadErrors": int(raw.get("eventLoadErrors") or 0),
}
intervals = []
times = []
for kill in kills:
    stamp = kill.get("at") or ""
    try:
        times.append(datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp())
    except Exception:
        pass
for a, b in zip(times, times[1:]):
    intervals.append(round(b - a, 3))
mean_interval = round(sum(intervals) / len(intervals), 3) if intervals else None
min_interval = min(intervals) if intervals else None
max_interval = max(intervals) if intervals else None

if chaos_exit != 0:
    failures.append(f"chaos harness exit={chaos_exit} (see chaos.log)")
if raw.get("mode") != "kill":
    failures.append(f"mode = {raw.get('mode')!r}, want 'kill'")
if not raw.get("pass"):
    failures.append(f"chaos verdict pass={raw.get('pass')!r}; failures={raw.get('failures')}")
if raw.get("failures"):
    failures.append(f"chaos reported failures: {raw.get('failures')}")
if faults < expected_kills - 1 or faults > expected_kills + 1:
    failures.append(f"faults = {faults}, want {expected_kills} +/- 1 for {duration:.0f}s at {kill_every:.0f}s")
if restarts != faults:
    failures.append(f"restarts = {restarts}, want == faults ({faults})")
if len(kills) != faults:
    failures.append(f"kill log has {len(kills)} entries, want {faults}")
if len(times) != faults:
    failures.append(f"kill log has {len(times)} parseable timestamps, want {faults}")
if intervals:
    # The loop uses a ticker, so intervals are the interval plus scheduler
    # jitter; a gap much larger than the interval means the harness stalled.
    if min_interval < kill_every * 0.8:
        failures.append(f"minimum kill interval {min_interval}s is below {kill_every*0.8:.1f}s")
    if max_interval > kill_every + 5:
        failures.append(f"maximum kill interval {max_interval}s exceeds {kill_every+5:.1f}s")
if int(raw.get("sessionsChecked") or 0) != sessions:
    failures.append(f"sessionsChecked = {raw.get('sessionsChecked')}, want {sessions}")
if int(raw.get("sessions") or 0) != sessions:
    failures.append(f"sessions = {raw.get('sessions')}, want {sessions}")
if int(raw.get("eventCount") or 0) <= 0:
    failures.append("eventCount = 0 (the ledger audit saw no events)")
if float(raw.get("durationSec") or 0) < duration:
    failures.append(f"durationSec = {raw.get('durationSec')}, want >= {duration}")
for name, value in counters.items():
    if value != 0:
        failures.append(f"{name} = {value}, want 0")

summary = dict(raw)
summary.update({
    "mode": "g5-chaos",
    "expectedKills": expected_kills,
    "killIntervalMeanSec": mean_interval,
    "killIntervalMinSec": min_interval,
    "killIntervalMaxSec": max_interval,
    "invariantCounters": counters,
    "verdict": "FAIL" if failures else "PASS",
    "failures": failures,
})
with open(out_path, "w") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
    fh.write("\n")

print(json.dumps({k: summary.get(k) for k in (
    "mode", "faults", "restarts", "durationSec", "sessions",
    "eventCount", "sessionsChecked", "invariantCounters",
    "killIntervalMeanSec", "killIntervalMinSec", "killIntervalMaxSec",
    "verdict",
)}, ensure_ascii=False, indent=2))
for failure in failures:
    print(f"G5 CHAOS FAIL: {failure}")
print(f"result JSON: {out_path}")
if failures:
    sys.exit(1)
print(f"G5 CHAOS PASS: {faults} SIGKILLs, 0 lost events, 0 duplicated final replies")
PY

# A ten-minute soak writes several MB of runtime logs; the evidence keeps the
# full compressed stream plus a readable tail so the report and code review do
# not need to inflate a multi-MB blob in git.
if [ -f "$OUT_DIR/runtime.log" ]; then
  LOG_SIZE=$(wc -c <"$OUT_DIR/runtime.log" | tr -d ' ')
  if [ "$LOG_SIZE" -gt 5242880 ]; then
    gzip -c "$OUT_DIR/runtime.log" >"$OUT_DIR/runtime.log.gz"
    tail -n 2000 "$OUT_DIR/runtime.log" >"$OUT_DIR/runtime-tail.log"
    rm -f "$OUT_DIR/runtime.log"
    say "   runtime log compressed ($LOG_SIZE bytes -> runtime.log.gz; tail in runtime-tail.log)"
  fi
fi

say "== chaos profile done (cleanup follows) =="
