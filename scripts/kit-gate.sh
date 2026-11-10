#!/usr/bin/env bash
# kit-gate.sh — the keel UI kit's door.
#
# A kit component ships only if it passes this script: both apps type-check,
# the TUI suite stays green (the existing App.tui.test.tsx plus the gallery's
# own case), the gallery packages for the web, and the packaged page is
# photographed at 390 / 768 / 1280 with the manifest-driven coverage assertion
# (`web/kit/gen-registry.mjs --assert-page`): every name of the kit's manifest
# is on the page, unclipped, and backed by a real export.
#
# Usage (from anywhere):
#   bash scripts/kit-gate.sh
#
# Environment:
#   KIT_SHOTS_DIR        where the screenshots, probe dumps and step logs land
#                        (default /tmp/kit-shots)
#   KIT_GATE_PORT_FIRST  first port to try (default 13784)
#   KIT_GATE_PORT_LAST   last port to try (default 13789)
#   KIT_GATE_SESSION     agent-browser session name (default keel-kit-gate-$$)
#
# Port rule: the kit gate serves the packaged gallery on 127.0.0.1 from the
# keel-ui-kit slice of the project's 13610–13799 range (13784–13789), taking
# the first free port and never binding anything else. It is a loopback-only
# HTTP dev server, which the project exempts from PORT.md registration
# (Owner decision 2026-11-03); nothing is exposed through frp.
#
# Exit code: 0 only when every step passed and the gallery covers the whole
# manifest. A missing component is the expected failure of a kit that has not
# landed yet: the script still runs to the end and prints the missing list.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEB="$ROOT/web"
KIT="$WEB/kit"
SHOTS="${KIT_SHOTS_DIR:-/tmp/kit-shots}"
PORT_FIRST="${KIT_GATE_PORT_FIRST:-13784}"
PORT_LAST="${KIT_GATE_PORT_LAST:-13789}"
SESSION="${KIT_GATE_SESSION:-keel-kit-gate-$$}"
VIEWPORTS=(390 768 1280)
VIEWPORT_HEIGHTS=(844 1024 800)

VERDICTS=()
SERVER_PID=""
PORT=""

say() { printf '%s\n' "$*"; }

cleanup() {
  local code=$?
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    sleep 0.3
    kill -9 "$SERVER_PID" 2>/dev/null || true
    say "kit-gate: killed static server pid $SERVER_PID (port $PORT)"
  fi
  if command -v agent-browser >/dev/null 2>&1; then
    agent-browser close --session "$SESSION" >/dev/null 2>&1 || true
    say "kit-gate: closed agent-browser session $SESSION"
  fi
  if [ -n "$PORT" ] && lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    say "kit-gate: WARNING port $PORT is still listening after cleanup:"
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN | sed 's/^/    /'
  fi
  return $code
}
trap cleanup EXIT INT TERM

# step <name> <cmd...>: run a gate step, keep its log, record the verdict.
step() {
  local name="$1"
  shift
  local log="$SHOTS/$name.log"
  say "-- $name: $*"
  if "$@" >"$log" 2>&1; then
    VERDICTS+=("$name=PASS")
    tail -n 2 "$log" | sed 's/^/   /'
    return 0
  fi
  VERDICTS+=("$name=FAIL")
  say "   FAIL — tail of $log:"
  tail -n 15 "$log" | sed 's/^/   /'
  return 1
}

in_dir() {
  local dir="$1"
  shift
  ( cd "$dir" && "$@" )
}

pick_port() {
  local port
  for ((port = PORT_FIRST; port <= PORT_LAST; port++)); do
    if ! lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
      printf '%s' "$port"
      return 0
    fi
  done
  return 1
}

for tool in nsh node python3 curl agent-browser; do
  command -v "$tool" >/dev/null 2>&1 || {
    say "kit-gate: missing tool: $tool"
    exit 2
  }
done

mkdir -p "$SHOTS"
# A stale artifact is a lying artifact: this run owns the directory's outputs.
rm -f "$SHOTS"/*.png "$SHOTS"/*.json "$SHOTS"/*.log "$SHOTS"/probe.js

say "kit-gate: root=$ROOT shots=$SHOTS"

# 1. The registry: the gallery's optional imports, resolved against what the
#    kit exports right now (see web/kit/gen-registry.mjs).
say "-- registry: node web/kit/gen-registry.mjs"
if node "$KIT/gen-registry.mjs" >"$SHOTS/registry.log" 2>&1; then
  VERDICTS+=("registry=PASS")
  sed 's/^/   /' "$SHOTS/registry.log"
else
  VERDICTS+=("registry=FAIL")
  tail -n 15 "$SHOTS/registry.log" | sed 's/^/   /'
fi

# 2. Both apps type-check. `nsh check` in web/ also covers web/kit's sources
#    (its tsconfig includes **/*.tsx); the kit app is checked on its own too,
#    because it has its own entry, theme resolution and .nsh/types.
step check-web in_dir "$WEB" nsh check || true
step check-kit in_dir "$KIT" nsh check || true

# 3. The TUI suite: the existing App.tui.test.tsx cases plus the gallery's own
#    case (nsh test finds *.test.tsx under web/, web/kit/ included).
step tui-web in_dir "$WEB" nsh test --target tui || true

# 4. The gallery builds for the web.
if ! step package-kit in_dir "$KIT" nsh package --target web -o dist; then
  say ""
  say "kit-gate: the gallery did not build; skipping the browser half."
  say "kit-gate: verdicts: ${VERDICTS[*]}"
  exit 1
fi

SITE="$(find "$KIT/dist" -maxdepth 2 -name index.html -print -quit | xargs -I{} dirname {})"
if [ -z "$SITE" ] || [ ! -d "$SITE" ]; then
  say "kit-gate: no packaged site under $KIT/dist"
  exit 1
fi
say "-- serve: $SITE"

PORT="$(pick_port)" || {
  say "kit-gate: no free port in $PORT_FIRST..$PORT_LAST"
  exit 1
}

python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$SITE" >"$SHOTS/server.log" 2>&1 &
SERVER_PID=$!
say "kit-gate: static server pid $SERVER_PID on http://127.0.0.1:$PORT/"

ready=0
for _ in $(seq 1 50); do
  if curl -fs -o /dev/null "http://127.0.0.1:$PORT/"; then
    ready=1
    break
  fi
  sleep 0.2
done
if [ "$ready" != 1 ]; then
  say "kit-gate: the static server never answered"
  exit 1
fi

# 5. The probe: the manifest's names, embedded by gen-registry.mjs, piped into
#    the browser. Each viewport gets a screenshot and a probe dump.
node "$KIT/gen-registry.mjs" --emit-eval "$SHOTS/probe.js" >"$SHOTS/probe.log" 2>&1 || {
  say "kit-gate: could not emit the page probe"
  tail -n 10 "$SHOTS/probe.log" | sed 's/^/   /'
  exit 1
}

ab() { agent-browser --session "$SESSION" "$@"; }

if ! ab open "http://127.0.0.1:$PORT/" >"$SHOTS/browser-open.log" 2>&1; then
  say "kit-gate: agent-browser could not open the gallery"
  tail -n 10 "$SHOTS/browser-open.log" | sed 's/^/   /'
  exit 1
fi
if ! ab wait --text "Keel UI Kit Gallery" >"$SHOTS/browser-wait.log" 2>&1; then
  say "kit-gate: the gallery page never rendered its title"
  tail -n 10 "$SHOTS/browser-wait.log" | sed 's/^/   /'
  exit 1
fi
say "kit-gate: agent-browser session $SESSION opened the gallery"

PROBE_DUMPS=()
SHOT_FILES=()
for index in "${!VIEWPORTS[@]}"; do
  width="${VIEWPORTS[$index]}"
  height="${VIEWPORT_HEIGHTS[$index]}"
  shot="$SHOTS/kit-${width}x${height}.png"
  dump="$SHOTS/probe-${width}x${height}.json"
  step "viewport-${width}" ab set viewport "$width" "$height" || true
  ab wait 600 >/dev/null 2>&1 || true
  step "shot-${width}" ab screenshot "$shot" || true
  if ab eval --stdin <"$SHOTS/probe.js" >"$dump" 2>"$SHOTS/eval-${width}.log"; then
    VERDICTS+=("probe-${width}=PASS")
    say "   probe-${width}: $dump ($(wc -c <"$dump" | tr -d ' ') bytes)"
    PROBE_DUMPS+=("$dump")
  else
    VERDICTS+=("probe-${width}=FAIL")
    say "   FAIL — tail of $SHOTS/eval-${width}.log:"
    tail -n 10 "$SHOTS/eval-${width}.log" | sed 's/^/   /'
  fi
  SHOT_FILES+=("$shot")
done

# The review copy: one screenshot per manifest group at 1280, scrolled to the
# group's heading (NSH scrolls an inner element, so the document has no full
# height to capture: the group shots are the readable evidence that every card
# rendered, and the probe files above are the machine-checkable one).
if node "$KIT/gen-registry.mjs" --emit-sections "$SHOTS/sections.json" >"$SHOTS/sections.log" 2>&1; then
  index=0
  while read -r label; do
    index=$((index + 1))
    slug=$(printf '%02d' "$index")
    scroll="$SHOTS/scroll-${slug}.js"
    node "$KIT/gen-registry.mjs" --emit-section "$label" "$scroll" >/dev/null 2>&1 || continue
    ab set viewport 1280 800 >/dev/null 2>&1 || true
    if ! ab eval --stdin <"$scroll" >"$SHOTS/scroll-${slug}.out" 2>&1; then
      say "   group ${slug} (${label}): could not scroll"
      continue
    fi
    ab wait 300 >/dev/null 2>&1 || true
    if ab screenshot "$SHOTS/kit-1280-group-${slug}.png" >/dev/null 2>&1; then
      say "   group ${slug} (${label}) → $SHOTS/kit-1280-group-${slug}.png"
    else
      say "   group ${slug} (${label}): screenshot failed"
    fi
  done < <(node -e 'const fs=require("fs");const rows=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));for(const row of rows)console.log(row.label);' "$SHOTS/sections.json")
else
  say "   could not emit the group list"
fi

# 6. Coverage and geometry: names, missing markers, label boxes, clipping,
#    horizontal overflow — all from the probe dumps, all manifest-driven.
if [ "${#PROBE_DUMPS[@]}" -gt 0 ]; then
  step coverage node "$KIT/gen-registry.mjs" --assert-page "${PROBE_DUMPS[@]}" || true
else
  VERDICTS+=("coverage=FAIL")
  say "kit-gate: no probe dump to assert on"
fi

say ""
say "kit-gate: screenshots:"
for shot in "${SHOT_FILES[@]}"; do
  [ -f "$shot" ] && say "  $shot"
done
for shot in "$SHOTS"/kit-1280-group-*.png; do
  [ -f "$shot" ] && say "  $shot"
done
say "kit-gate: verdicts: ${VERDICTS[*]}"
if [ -f "$SHOTS/coverage.log" ] && grep -q "kit-coverage: missing from the kit" "$SHOTS/coverage.log"; then
  grep "kit-coverage: missing from the kit" "$SHOTS/coverage.log" | sed 's/^/kit-gate: /'
fi

verdict_fail=0
for verdict in "${VERDICTS[@]}"; do
  case "$verdict" in
    *=FAIL) verdict_fail=1 ;;
  esac
done
if [ "$verdict_fail" = 1 ]; then
  say "kit-gate: FAIL"
  exit 1
fi
say "kit-gate: PASS"
exit 0
