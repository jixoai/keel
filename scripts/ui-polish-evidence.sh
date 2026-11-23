#!/usr/bin/env bash
# ui-polish-evidence.sh — package the web client, serve it on a loopback port,
# and run the UI-polish lane's browser gate (scripts/ui-polish-e2e.mjs).
#
# The pattern is scripts/system-bar-evidence.sh's: `nsh package --target web`, a
# loopback python static server on a port from the project's 13610–13799 range,
# and headless Chrome driven by playwright-core. It writes the screenshots and
# report.json; it writes no HTML report. The fixture transport is the only
# launch parameter, so no credential is involved.
#
# Usage:
#   bash scripts/ui-polish-evidence.sh                      # build + after/
#   UI_POLISH_EVIDENCE=…/before UI_POLISH_SKIP_BUILD=1 \
#     UI_POLISH_BUILD=/tmp/keel-ui-polish-before \
#     bash scripts/ui-polish-evidence.sh                    # a baseline build
#
# Environment:
#   UI_POLISH_EVIDENCE    output directory
#                         (default openspec/changes/keel-harness-v2/evidence/ui-polish/after)
#   UI_POLISH_PORT        first loopback port to try (default 13773)
#   UI_POLISH_BUILD       build directory (default /tmp/keel-ui-polish-web)
#   UI_POLISH_SKIP_BUILD  1 to use an existing build at UI_POLISH_BUILD
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEB="$ROOT/web"
EVIDENCE="${UI_POLISH_EVIDENCE:-$ROOT/openspec/changes/keel-harness-v2/evidence/ui-polish/after}"
BUILD="${UI_POLISH_BUILD:-/tmp/keel-ui-polish-web}"
PORT_FIRST="${UI_POLISH_PORT:-13773}"
SKIP_BUILD="${UI_POLISH_SKIP_BUILD:-0}"
SERVER_PID=""
PORT=""

say() { printf '%s\n' "$*"; }

cleanup() {
  local code=$?
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    sleep 0.3
    kill -9 "$SERVER_PID" 2>/dev/null || true
    say "ui-polish: killed static server pid $SERVER_PID (port $PORT)"
  fi
  if [ -n "$PORT" ] && lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    say "ui-polish: WARNING port $PORT is still listening after cleanup:"
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN | sed 's/^/    /'
  fi
  return $code
}
trap cleanup EXIT INT TERM

for tool in nsh node python3 curl lsof; do
  command -v "$tool" >/dev/null 2>&1 || {
    say "ui-polish: missing tool: $tool"
    exit 2
  }
done

mkdir -p "$EVIDENCE"
# A stale artifact is a lying artifact: this run owns the report and shots it
# writes. Another directory (before/) is untouched.
rm -f "$EVIDENCE"/*.png "$EVIDENCE"/report.json "$EVIDENCE"/server.log

if [ "$SKIP_BUILD" != "1" ]; then
  rm -rf "$BUILD"
  say "-- package: nsh package --target web -o $BUILD"
  if ! ( cd "$WEB" && nsh package --target web -o "$BUILD" ) >"$EVIDENCE/package.log" 2>&1; then
    say "ui-polish: the web build failed (tail):"
    tail -n 20 "$EVIDENCE/package.log" | sed 's/^/   /'
    exit 1
  fi
  tail -n 2 "$EVIDENCE/package.log" | sed 's/^/   /'
else
  say "-- package: skipped (using $BUILD)"
fi

SITE="$(find "$BUILD" -maxdepth 2 -name index.html -print -quit | xargs -I{} dirname {})"
if [ -z "$SITE" ] || [ ! -d "$SITE" ]; then
  say "ui-polish: no packaged site under $BUILD"
  exit 1
fi

for ((candidate = PORT_FIRST; candidate <= PORT_FIRST + 6; candidate++)); do
  if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
    PORT="$candidate"
    break
  fi
done
if [ -z "$PORT" ]; then
  say "ui-polish: no free loopback port in $PORT_FIRST..$((PORT_FIRST + 6))"
  exit 1
fi

python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$SITE" >"$EVIDENCE/server.log" 2>&1 &
SERVER_PID=$!
say "ui-polish: serving $SITE at http://127.0.0.1:$PORT/ (pid $SERVER_PID)"
for _ in $(seq 1 50); do
  curl -fs -o /dev/null "http://127.0.0.1:$PORT/" && break
  sleep 0.2
done
if ! curl -fs -o /dev/null "http://127.0.0.1:$PORT/"; then
  say "ui-polish: the static server never answered"
  exit 1
fi

E2E_BASE="http://127.0.0.1:$PORT" E2E_EVIDENCE="$EVIDENCE" node "$ROOT/scripts/ui-polish-e2e.mjs"
STATUS=$?
say "ui-polish: evidence: $EVIDENCE"
say "ui-polish: report: $EVIDENCE/report.json"
for file in "$EVIDENCE"/*.png; do
  [ -f "$file" ] && say "  $file"
done
exit "$STATUS"
