#!/usr/bin/env bash
# Real-browser degradation E2E for MCP Apps (change feat-mcp-apps-events,
# P6A2a). It reuses the P6A1 startup chain (real PostgreSQL, real `keel serve`,
# real stdio MCP fixture, real Chrome) and adds two failure scenarios:
#
#   handshake  ui://weather/broken never answers ui/initialize. Assert the row
#              becomes the readable text fallback within the host's 5 s
#              deadline, the broken iframe is gone, the retry restarts the
#              handshake, and the good app in the same session keeps its
#              instance and its app-only tools/call result.
#
#   csp        ui://weather/leaky declares only https://api.fixture.test in
#              _meta.ui.csp.connectDomains and fetches
#              https://evil.fixture.test/leak. Assert the browser blocks it
#              (in-frame securitypolicyviolation + console), a local TCP trap
#              mapped to evil.fixture.test receives zero connections while the
#              app runs (and at least one in a negative control), the document
#              CSP is exact, and the host page does not navigate.
#
# Each scenario has its own database, its own serve/stub/trap processes and its
# own evidence directory. Everything is torn down on exit, including on
# failure; the port release is written to evidence.
#
# Usage:
#   scripts/mcp-apps-degrade-e2e.sh [--scenario handshake|csp|both]
#                                   [--evidence DIR] [--skip-web-build]
#
# The exit status is non-zero when any scenario fails or the report merge
# fails. Ports: 13682 (serve), 13683 (stub provider), 13685 (CSP trap).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
EVIDENCE="$ROOT/openspec/changes/feat-mcp-apps-events/evidence/p6/degrade"
SCENARIO_ARG="both"
SKIP_WEB_BUILD=0
SERVE_PORT="${MCP_E2E_SERVE_PORT:-13682}"
STUB_PORT="${MCP_E2E_STUB_PORT:-13683}"
LEAK_PORT="${MCP_E2E_LEAK_PORT:-13685}"
LEAK_HOST="${MCP_E2E_LEAK_HOST:-evil.fixture.test}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --scenario) SCENARIO_ARG="$2"; shift 2 ;;
    --evidence) EVIDENCE="$2"; shift 2 ;;
    --skip-web-build) SKIP_WEB_BUILD=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$SCENARIO_ARG" in
  both) SCENARIOS=(handshake csp) ;;
  handshake|csp) SCENARIOS=("$SCENARIO_ARG") ;;
  *) echo "--scenario must be handshake, csp or both" >&2; exit 2 ;;
esac

log() { printf '[mcp-apps-degrade-e2e] %s\n' "$*"; }
die() { printf '[mcp-apps-degrade-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

mkdir -p "$EVIDENCE"

# --- credentials/env -------------------------------------------------------
# Only KEY-shaped assignments are read; nothing is echoed. The test DSN is the
# dedicated database's server connection, never the product database.
ENV_FILE="${FDE2E_ENV_PERSONAL:-$ROOT/.env.local}"
if [[ ! -f "$ENV_FILE" ]]; then
  ENV_FILE="$ROOT/../../.env.personal"
fi
[[ -f "$ENV_FILE" ]] || die "no env file found (.env.local or the main repo's .env.personal)"

DSN=""
while IFS= read -r line; do
  key="${line%%=*}"; value="${line#*=}"
  value="${value%\"}"; value="${value#\"}"; value="${value%\'}"; value="${value#\'}"
  case "$key" in
    KEEL_TEST_DSN) DSN="$value" ;;
    KEEL_DSN) [[ -z "$DSN" ]] && DSN="$value" ;;
    KEEL_SERVE_PASSWORD) : ;; # retired; the lane provisions an account below
  esac
done < <(grep -E '^(KEEL_TEST_DSN|KEEL_DSN|KEEL_SERVE_PASSWORD)=' "$ENV_FILE")
[[ -n "$DSN" ]] || die "KEEL_TEST_DSN is not set in $ENV_FILE"

source "$ROOT/scripts/lib/keel-auth.sh"

# The PostgreSQL container is the one publishing the DSN's port.
DSN_PORT="$(KEEL_DSN_VALUE="$DSN" python3 - <<'PY'
import os, re
dsn = os.environ["KEEL_DSN_VALUE"]
match = re.match(r"postgres(?:ql)?://([^@]*@)?([^/:?]+)(:[0-9]+)?", dsn)
print((match.group(3) or ":5432").lstrip(":") if match else "5432")
PY
)"
PG_CTR="$(docker ps --format '{{.Names}}\t{{.Ports}}' | awk -F'\t' -v port=":$DSN_PORT->" 'index($2, port) { print $1; exit }')"
[[ -n "$PG_CTR" ]] || die "no postgres container publishes port $DSN_PORT"
log "postgres container: $PG_CTR (port $DSN_PORT)"

dsn_for_db() { # DBNAME → DSN on stdout
  KEEL_DSN_VALUE="$DSN" DB_NAME="$1" python3 - <<'PY'
import os, re
dsn = os.environ["KEEL_DSN_VALUE"]
print(re.sub(r"/[^/?]+(\?|$)", "/" + os.environ["DB_NAME"] + r"\1", dsn, count=1))
PY
}

port_free() {
  # SO_REUSEADDR mirrors what the Go server and Python's TCPServer do: a
  # TIME_WAIT connection must not count as "in use", an active listener does.
  python3 - "$1" <<'PY'
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
}

wait_port() { # PORT PID LABEL
  local port="$1" pid="$2" label="$3"
  for _ in $(seq 1 100); do
    if ! port_free "$port"; then return 0; fi
    kill -0 "$pid" 2>/dev/null || die "$label exited before listening on $port"
    sleep 0.1
  done
  die "$label did not start on port $port"
}

for p in "$SERVE_PORT" "$STUB_PORT" "$LEAK_PORT"; do
  port_free "$p" || die "port $p is already in use"
done

# --- build (once) ----------------------------------------------------------
KEEL_BIN="${KEEL_BIN:-/tmp/keel-mcp-degrade-e2e-$STAMP}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

WEB_DIR="$ROOT/web/dist/web/keel/Keel"
if [[ "$SKIP_WEB_BUILD" != 1 || ! -f "$WEB_DIR/index.html" ]]; then
  log "building the web target (nsh package)"
  ( cd "$ROOT/web" && nsh package --target web -o dist/web/keel ) > "$EVIDENCE/web-build.log" 2>&1
fi
[[ -f "$WEB_DIR/index.html" ]] || die "web build has no index.html at $WEB_DIR"

# --- per-scenario lifecycle ------------------------------------------------
SERVE_PID=""
STUB_PID=""
LEAK_PID=""
DB=""
RUN_DIR=""
CLEANED=1

cleanup_current() {
  [[ "$CLEANED" == 1 ]] && return 0
  CLEANED=1
  for pid_var in SERVE_PID STUB_PID LEAK_PID; do
    local pid="${!pid_var}"
    if [[ -n "$pid" ]]; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  local db_state="no database was created"
  if [[ -n "$DB" ]]; then
    docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1 || true
    if docker exec "$PG_CTR" psql -U postgres -Atc "select 1 from pg_database where datname = '$DB'" 2>/dev/null | grep -q 1; then
      db_state="database $DB STILL EXISTS"
    else
      db_state="database $DB dropped (absent from pg_database)"
    fi
  fi
  # The MCP fixture is a child of serve; pkill by the worktree path is the
  # belt-and-braces check the port probe cannot make for a stdio child.
  pkill -f "$ROOT/scripts/mcp-apps-e2e/mcp-server.mjs" 2>/dev/null || true
  # The session cookie is credential-shaped: never kept.
  if [[ -n "$RUN_DIR" ]]; then
    rm -f "$RUN_DIR/cookies.txt" "$RUN_DIR/csrf.txt" "$RUN_DIR/auth.json" \
      "$RUN_DIR/setup.token" "$RUN_DIR/login-body.json"
  fi
  sleep 0.4
  if [[ -n "$RUN_DIR" ]]; then
    {
      echo "# cleanup check"
      echo "$db_state"
      if pgrep -f "$ROOT/scripts/mcp-apps-e2e/mcp-server.mjs" >/dev/null 2>&1; then
        echo "mcp fixture process STILL RUNNING"
      else
        echo "mcp fixture process: none"
      fi
      echo "# port release check"
      for p in "$SERVE_PORT" "$STUB_PORT" "$LEAK_PORT"; do
        if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
          echo "port $p STILL LISTENING"
        else
          echo "port $p released (no listener)"
        fi
      done
    } > "$RUN_DIR/port-release.txt" 2>&1 || true
  fi
  SERVE_PID=""
  STUB_PID=""
  LEAK_PID=""
  DB=""
  RUN_DIR=""
}
trap 'rc=$?; cleanup_current; trap - EXIT; exit $rc' EXIT
trap 'exit 130' INT TERM

api() { # METHOD PATH [BODY] [JAR] → status on stdout, body in last-body.json
  local method="$1" path="$2" body="${3:-}" jar="${4:-$RUN_DIR/cookies.txt}"
  local base="http://127.0.0.1:$SERVE_PORT"
  local -a args=(-sS -b "$jar" -c "$jar" -X "$method")
  if [[ -s "$RUN_DIR/csrf.txt" ]]; then
    args+=(-H "X-Keel-CSRF: $(cat "$RUN_DIR/csrf.txt")")
  fi
  if [[ -n "$body" ]]; then
    curl "${args[@]}" "$base$path" -H 'Content-Type: application/json' \
      --data-binary "$body" -o "$RUN_DIR/last-body.json" -w '%{http_code}'
  else
    curl "${args[@]}" "$base$path" -o "$RUN_DIR/last-body.json" -w '%{http_code}'
  fi
}

last_json() { python3 -c 'import json,sys;print(json.dumps(json.load(open(sys.argv[1])), ensure_ascii=False))' "$RUN_DIR/last-body.json"; }

run_scenario() { # NAME
  local name="$1"
  RUN_DIR="$EVIDENCE/$name"
  mkdir -p "$RUN_DIR/screenshots"
  DB="keel_degrade_e2e_${STAMP}_${name}"
  DB="${DB//-/_}"
  CLEANED=0
  log "scenario $name: database $DB, evidence $RUN_DIR"
  for p in "$SERVE_PORT" "$STUB_PORT" "$LEAK_PORT"; do
    port_free "$p" || die "port $p is still in use before scenario $name"
  done

  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null
  docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE \"$DB\"" >/dev/null

  # MCP fixture config: a real stdio child with the fixture log path and the
  # P6A2a degradation apps enabled.
  cat > "$RUN_DIR/mcp.json" <<JSON
{
  "mcpServers": {
    "e2e": {
      "command": "node",
      "args": ["$ROOT/scripts/mcp-apps-e2e/mcp-server.mjs"],
      "env": {"MCP_E2E_LOG": "$RUN_DIR/mcp-calls.jsonl", "MCP_E2E_DEGRADE": "1"},
      "description": "P6A2a degradation fixture (broken + leaky apps)"
    }
  }
}
JSON

  # The recording stub provider drives one aggregate mcp call per turn until
  # the scenario's sequence is exhausted, then a final answer.
  local sequence="render_weather,render_broken"
  [[ "$name" == "csp" ]] && sequence="render_leaky"
  cat > "$RUN_DIR/models-config.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "kite-e2e",
      "name": "MCP Apps degrade E2E (recording stub)",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_E2E_KEY"},
      "modelIds": ["kite-e2e-model"],
      "visibility": "visible"
    }
  ],
  "selection": {"providerId": "kite-e2e", "modelId": "kite-e2e-model"}
}
JSON

  : > "$RUN_DIR/wire-requests.jsonl"
  : > "$RUN_DIR/stub-provider.log"
  : > "$RUN_DIR/mcp-calls.jsonl"
  MCP_E2E_TOOL_SEQUENCE="$sequence" python3 "$ROOT/scripts/mcp-apps-e2e/stub-provider.py" \
    "$STUB_PORT" "$RUN_DIR/wire-requests.jsonl" > "$RUN_DIR/stub-provider.log" 2>&1 &
  STUB_PID=$!
  wait_port "$STUB_PORT" "$STUB_PID" "stub provider"

  if [[ "$name" == "csp" ]]; then
    : > "$RUN_DIR/leak-trap.jsonl"
    python3 "$ROOT/scripts/mcp-apps-e2e/leak-listener.py" "$LEAK_PORT" \
      "$RUN_DIR/leak-trap.jsonl" > "$RUN_DIR/leak-listener.log" 2>&1 &
    LEAK_PID=$!
    wait_port "$LEAK_PORT" "$LEAK_PID" "CSP trap listener"
  fi

  TEST_DSN="$(dsn_for_db "$DB")"
  local tenant session
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  session="$(uuidgen | tr '[:upper:]' '[:lower:]')"

  KITE_E2E_KEY="kite-e2e-not-a-credential" \
  KEEL_DSN="$TEST_DSN" KEEL_TEST_DSN="$TEST_DSN" \
  KEEL_SECRET_KEY="$(openssl rand -hex 32)" \
  KEEL_MODELS_CONFIG="$RUN_DIR/models-config.json" \
    "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$tenant" \
      --web-dir "$WEB_DIR" --mcp "$RUN_DIR/mcp.json" > "$RUN_DIR/serve.log" 2>&1 &
  SERVE_PID=$!
  for _ in $(seq 1 150); do
    if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
    kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $RUN_DIR/serve.log)"
    sleep 0.1
  done
  curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

  # Each scenario has its own database, so it also bootstraps its own account.
  keel_auth_provision "http://127.0.0.1:$SERVE_PORT" "$KEEL_BIN" "$TEST_DSN" "$tenant" "$RUN_DIR"
  local jar="$KEEL_E2E_JAR"

  local code
  code="$(api GET /v1/capabilities "" "$jar")"
  [[ "$code" == "200" ]] || die "capabilities: HTTP $code"
  cp "$RUN_DIR/last-body.json" "$RUN_DIR/capabilities.json"
  python3 - "$RUN_DIR/capabilities.json" <<'PY'
import json, sys
features = json.load(open(sys.argv[1])).get("features") or {}
for key in ("mcp.apps", "mcp.apps.tools", "mcp.apps.resources"):
    assert features.get(key) is True, (key, features)
print("capabilities ok")
PY

  code="$(api POST /v1/sessions "$(python3 -c 'import json,sys;print(json.dumps({"sessionId":sys.argv[1]}))' "$session")" "$jar")"
  [[ "$code" == "201" ]] || die "create session: HTTP $code $(last_json)"

  local text="请展示上海天气面板，并触发坏面板的握手超时"
  [[ "$name" == "csp" ]] && text="请展示泄漏夹具，验证未声明域名的请求被阻断"
  local input_body
  input_body="$(python3 - "$session" "$text" <<'PY'
import json, sys
print(json.dumps({
    "text": sys.argv[2],
    "requestId": "mcp-apps-degrade-%s-turn-1" % sys.argv[1],
    "selection": {"providerId": "kite-e2e", "modelId": "kite-e2e-model"},
}, ensure_ascii=False))
PY
)"
  code="$(api POST "/v1/sessions/$session/input" "$input_body" "$jar")"
  [[ "$code" == "201" ]] || die "input: HTTP $code $(last_json)"

  local status=""
  for _ in $(seq 1 300); do
    api GET "/v1/sessions/$session/run" "" "$jar" >/dev/null
    status="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("status",""))' "$RUN_DIR/last-body.json")"
    case "$status" in done|failed|cancelled) break ;; esac
    sleep 0.2
  done
  [[ "$status" == "done" ]] || die "run settled as ${status:-unknown} (see $RUN_DIR/serve.log)"
  cp "$RUN_DIR/last-body.json" "$RUN_DIR/run.json"

  api GET "/v1/sessions/$session/events" "" "$jar" >/dev/null
  cp "$RUN_DIR/last-body.json" "$RUN_DIR/events.json"
  python3 - "$RUN_DIR/events.json" "$name" <<'PY'
import json, re, sys
name = sys.argv[2]
body = json.load(open(sys.argv[1]))
events = body.get("events", body if isinstance(body, list) else [])
results = [event for event in events if event.get("kind") == "tool.result"]
apps = [((event.get("payload") or {}).get("details") or {}).get("app") or {} for event in results]
uris = [app.get("resourceUri") for app in apps]
expected = {
    "handshake": ["ui://weather/dashboard", "ui://weather/broken"],
    "csp": ["ui://weather/leaky"],
}[name]
assert sorted(uris) == sorted(expected), "tool.result apps = %r, want %r" % (uris, expected)
for app in apps:
    assert re.match(r"^mcpapp_[a-f0-9]{16}$", app.get("appId") or ""), app
    visibility = app.get("visibility") or []
    assert "model" in visibility and "app" in visibility, app
print("tool.result bindings:", uris)
PY

  api GET /v1/mcp/apps "" "$jar" >/dev/null
  cp "$RUN_DIR/last-body.json" "$RUN_DIR/catalogue.json"
  python3 - "$RUN_DIR/catalogue.json" "$name" <<'PY'
import json, re, sys
name = sys.argv[2]
body = json.load(open(sys.argv[1]))
apps = body.get("apps") or []
by_uri = {app.get("resourceUri"): app for app in apps}
expected = ["ui://weather/dashboard", "ui://weather/broken", "ui://weather/leaky"]
assert sorted(by_uri) == sorted(expected), "catalogue = %r" % sorted(by_uri)
for uri in expected:
    app = by_uri[uri]
    assert app.get("server") == "e2e", app
    assert re.match(r"^mcpapp_[a-f0-9]{16}$", app.get("appId") or ""), app
    assert re.match(r"^[a-f0-9]{64}$", app.get("hash") or ""), app
    assert app.get("documentUrl") == "/v1/mcp/apps/%s/view?v=%s" % (app["appId"], app["hash"]), app
if name == "csp":
    csp = by_uri["ui://weather/leaky"].get("csp") or ""
    assert "connect-src 'self' https://api.fixture.test" in csp, csp
    assert "evil.fixture.test" not in csp, csp
print("catalogue apps:", sorted(by_uri))
PY

  # --- the real browser ----------------------------------------------------
  local browser_rc=0
  set +e
  E2E_BASE="http://127.0.0.1:$SERVE_PORT" \
  E2E_EVIDENCE="$RUN_DIR" \
  E2E_RUN="$name" \
  E2E_SCENARIO="$name" \
  E2E_MCP_LOG="$RUN_DIR/mcp-calls.jsonl" \
  E2E_WIRE_LOG="$RUN_DIR/wire-requests.jsonl" \
  E2E_LEAK_LOG="$RUN_DIR/leak-trap.jsonl" \
  E2E_LEAK_PORT="$LEAK_PORT" \
  E2E_LEAK_HOST="$LEAK_HOST" \
  E2E_USERNAME="$KEEL_E2E_USERNAME" \
  E2E_PASSWORD="$KEEL_E2E_PASSWORD" \
    timeout 240 node "$ROOT/scripts/mcp-apps-e2e/browser-degrade.mjs" > "$RUN_DIR/browser.log" 2>&1
  browser_rc=$?
  set -e
  if [[ "$browser_rc" -ne 0 ]]; then
    log "scenario $name browser failed (rc=$browser_rc); see $RUN_DIR/report.html"
    tail -40 "$RUN_DIR/browser.log" >&2 || true
    cleanup_current
    exit "$browser_rc"
  fi

  # Hygiene: the evidence must not contain a DSN, the account password or an
  # Authorization value. (The cookie jar, CSRF file and auth summary are
  # already removed by cleanup_current.)
  if grep -rqF "postgres://" "$RUN_DIR" 2>/dev/null; then
    die "evidence contains a DSN (scenario $name)"
  fi
  if grep -rqF "postgresql://" "$RUN_DIR" 2>/dev/null; then
    die "evidence contains a DSN (scenario $name)"
  fi
  if grep -rqF -- "$KEEL_E2E_PASSWORD" "$RUN_DIR" 2>/dev/null; then
    die "evidence contains the test account password (scenario $name)"
  fi

  cleanup_current
  log "scenario $name: PASS"
}

for name in "${SCENARIOS[@]}"; do
  run_scenario "$name"
done

# --- combined report -------------------------------------------------------
node "$ROOT/scripts/mcp-apps-e2e/degrade-report.mjs" "$EVIDENCE" "${SCENARIOS[@]}"

log "PASS — evidence in $EVIDENCE (report.html is the combined report)"
