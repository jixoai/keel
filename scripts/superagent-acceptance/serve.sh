#!/usr/bin/env bash
# Starts/stops the local super-agent acceptance harness without ever blocking
# the caller by default.
#
#   KEEL_TEST_DSN='postgres://...' scripts/superagent-acceptance/serve.sh start
#   scripts/superagent-acceptance/serve.sh wait   # foreground; Ctrl-C shuts down
#   scripts/superagent-acceptance/serve.sh stop   # kills the recorded pid
#
# start (default) launches the binary with nohup, records pid/log/state files
# and prints the pid + base URL, then exits. stop reclaims exactly that pid.
# The binary picks its own free port; the port is read from the state file.
set -euo pipefail

MODE="${1:-start}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${KEEL_SUPERAGENT_ACCEPTANCE_BIN:-/tmp/keel-superagent-acceptance}"
STATE="${KEEL_SUPERAGENT_ACCEPTANCE_STATE:-/tmp/keel-superagent-acceptance-state.json}"
PIDFILE="${KEEL_SUPERAGENT_ACCEPTANCE_PID:-${STATE}.pid}"
LOG="${KEEL_SUPERAGENT_ACCEPTANCE_LOG:-${STATE}.log}"

stop_pid() {
  local pid="$1"
  if ! kill -0 "$pid" 2>/dev/null; then
    rm -f "$PIDFILE"
    return 0
  fi
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$PIDFILE"
}

case "$MODE" in
  stop)
    if [ ! -f "$PIDFILE" ]; then
      echo "no pid file: $PIDFILE" >&2
      exit 1
    fi
    PID="$(cat "$PIDFILE")"
    stop_pid "$PID"
    echo "stopped pid $PID"
    exit 0
    ;;
  start|wait) ;;
  *)
    echo "usage: $0 [start|wait|stop]" >&2
    exit 2
    ;;
esac

: "${KEEL_TEST_DSN:?KEEL_TEST_DSN is required (local temporary PostgreSQL)}"
if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "already running: pid $(cat "$PIDFILE") (state $STATE)" >&2
  exit 1
fi

cd "$ROOT"
go build -o "$BIN" ./scripts/superagent-acceptance
rm -f "$STATE" "$LOG"
nohup "$BIN" >>"$LOG" 2>&1 &
PID=$!
printf '%s\n' "$PID" >"$PIDFILE"

cleanup() {
  stop_pid "$PID"
}
if [ "$MODE" = "wait" ]; then
  trap cleanup EXIT INT TERM
fi

for _ in $(seq 1 200); do
  if [ -f "$STATE" ]; then
    break
  fi
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "acceptance server exited before readiness; log:" >&2
    tail -20 "$LOG" >&2 || true
    rm -f "$PIDFILE"
    exit 1
  fi
  sleep 0.1
done
if [ ! -f "$STATE" ]; then
  echo "acceptance server did not become ready; log: $LOG" >&2
  stop_pid "$PID"
  exit 1
fi

BASE="$(jq -r .baseUrl "$STATE" 2>/dev/null || python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["baseUrl"])' "$STATE")"
echo "pid=$PID"
echo "base=$BASE"
echo "state=$STATE"
echo "log=$LOG"
echo "stop: KEEL_SUPERAGENT_ACCEPTANCE_STATE=$STATE $0 stop"
if [ "$MODE" = "wait" ]; then
  echo "foreground wait: Ctrl-C stops pid $PID"
  wait "$PID"
fi
