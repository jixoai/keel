#!/usr/bin/env bash
# system-bar-evidence.sh — package the web client, serve it on a loopback
# port, and run the shell's browser gate (scripts/system-bar-shell-e2e.mjs).
#
# This follows scripts/kit-gate.sh's pattern (the repo's existing browser
# evidence lane): `nsh package --target web`, a loopback python static server on
# a port from the project's 13610–13799 range, and a headless Chrome driven by
# playwright-core. It writes the screenshots and report.json; it writes no HTML
# report. The fixture transport is the only launch parameter, so no credential
# is involved.
#
# Usage:
#   bash scripts/system-bar-evidence.sh
#
# Environment:
#   SYSTEM_BAR_EVIDENCE   output directory
#                         (default openspec/changes/keel-harness-v2/evidence/system-bar/after)
#   SYSTEM_BAR_PORT       first loopback port to try (default 13763)
#   SYSTEM_BAR_SKIP_BUILD 1 to use an existing build at /tmp/keel-system-bar-web
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEB="$ROOT/web"
EVIDENCE="${SYSTEM_BAR_EVIDENCE:-$ROOT/openspec/changes/keel-harness-v2/evidence/system-bar/after}"
BUILD="${SYSTEM_BAR_BUILD:-/tmp/keel-system-bar-web}"
PORT_FIRST="${SYSTEM_BAR_PORT:-13763}"
SKIP_BUILD="${SYSTEM_BAR_SKIP_BUILD:-0}"
SERVER_PID=""
PORT=""

say() { printf '%s\n' "$*"; }

cleanup() {
  local code=$?
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    sleep 0.3
    kill -9 "$SERVER_PID" 2>/dev/null || true
    say "system-bar: killed static server pid $SERVER_PID (port $PORT)"
  fi
  if [ -n "$PORT" ] && lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    say "system-bar: WARNING port $PORT is still listening after cleanup:"
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN | sed 's/^/    /'
  fi
  return $code
}
trap cleanup EXIT INT TERM

for tool in nsh node python3 curl lsof; do
  command -v "$tool" >/dev/null 2>&1 || {
    say "system-bar: missing tool: $tool"
    exit 2
  }
done

mkdir -p "$EVIDENCE"
# A stale artifact is a lying artifact: this run owns the report and shots it
# writes (the before/ directory, captured at the baseline commit, is untouched).
rm -f "$EVIDENCE"/*.png "$EVIDENCE"/report.json "$EVIDENCE"/server.log

if [ "$SKIP_BUILD" != "1" ]; then
  rm -rf "$BUILD"
  say "-- package: nsh package --target web -o $BUILD"
  if ! ( cd "$WEB" && nsh package --target web -o "$BUILD" ) >"$EVIDENCE/package.log" 2>&1; then
    say "system-bar: the web build failed (tail):"
    tail -n 20 "$EVIDENCE/package.log" | sed 's/^/   /'
    exit 1
  fi
  tail -n 2 "$EVIDENCE/package.log" | sed 's/^/   /'
else
  say "-- package: skipped (using $BUILD)"
fi

SITE="$(find "$BUILD" -maxdepth 2 -name index.html -print -quit | xargs -I{} dirname {})"
if [ -z "$SITE" ] || [ ! -d "$SITE" ]; then
  say "system-bar: no packaged site under $BUILD"
  exit 1
fi

for ((candidate = PORT_FIRST; candidate <= PORT_FIRST + 6; candidate++)); do
  if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
    PORT="$candidate"
    break
  fi
done
if [ -z "$PORT" ]; then
  say "system-bar: no free loopback port in $PORT_FIRST..$((PORT_FIRST + 6))"
  exit 1
fi

python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$SITE" >"$EVIDENCE/server.log" 2>&1 &
SERVER_PID=$!
say "system-bar: serving $SITE at http://127.0.0.1:$PORT/ (pid $SERVER_PID)"
for _ in $(seq 1 50); do
  curl -fs -o /dev/null "http://127.0.0.1:$PORT/" && break
  sleep 0.2
done
if ! curl -fs -o /dev/null "http://127.0.0.1:$PORT/"; then
  say "system-bar: the static server never answered"
  exit 1
fi

E2E_BASE="http://127.0.0.1:$PORT" E2E_EVIDENCE="$EVIDENCE" node "$ROOT/scripts/system-bar-shell-e2e.mjs"
STATUS=$?
say "system-bar: evidence: $EVIDENCE"
say "system-bar: report: $EVIDENCE/report.json"
for file in "$EVIDENCE"/*.png; do
  [ -f "$file" ] && say "  $file"
done
exit "$STATUS"
