#!/usr/bin/env bash
# Real-browser E2E for MCP Apps (change feat-mcp-apps-events, P6.2 / P6A1).
#
# What it proves, against a real PostgreSQL database, a real `keel serve`, a
# real stdio MCP server and a real Chrome:
#
#   1. the production web build logs in, opens the session and expands the
#      aggregate `mcp` tool row;
#   2. the host mounts the app iframe at the hash-named view URL with a sandbox
#      that has no `allow-same-origin`, and serves the four security headers;
#   3. the app completes initialize → initialized → tool-input → tool-result;
#   4. the app's own `tools/call` (an app-only tool) reaches the MCP server
#      through the host proxy (the server log is the assertion);
#   5. fullscreen mounts a new instance and re-handshakes, and exiting mounts
#      another fresh instance.
#
# Each run has its own database (name contains e2e), ports 13682 (serve) and
# 13683 (stub provider), and its own evidence directory. Everything is torn
# down on exit, including on failure; the port release is written to evidence.
#
# Usage:
#   scripts/mcp-apps-e2e.sh [--runs N] [--evidence DIR] [--skip-web-build]
#
# Default runs=2: the acceptance requires two consecutive identical results in
# one invocation. The exit status is non-zero when any run fails or the two
# runs disagree on the asserted facts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
EVIDENCE="$ROOT/openspec/changes/feat-mcp-apps-events/evidence/p6/browser"
RUNS=2
SKIP_WEB_BUILD=0
SERVE_PORT="${MCP_E2E_SERVE_PORT:-13682}"
STUB_PORT="${MCP_E2E_STUB_PORT:-13683}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs) RUNS="$2"; shift 2 ;;
    --evidence) EVIDENCE="$2"; shift 2 ;;
    --skip-web-build) SKIP_WEB_BUILD=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[mcp-apps-e2e] %s\n' "$*"; }
die() { printf '[mcp-apps-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

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
SERVE_PASS=""
while IFS= read -r line; do
  key="${line%%=*}"; value="${line#*=}"
  value="${value%\"}"; value="${value#\"}"; value="${value%\'}"; value="${value#\'}"
  case "$key" in
    KEEL_TEST_DSN) DSN="$value" ;;
    KEEL_DSN) [[ -z "$DSN" ]] && DSN="$value" ;;
    KEEL_SERVE_PASSWORD) SERVE_PASS="$value" ;;
  esac
done < <(grep -E '^(KEEL_TEST_DSN|KEEL_DSN|KEEL_SERVE_PASSWORD)=' "$ENV_FILE")
[[ -n "$DSN" ]] || die "KEEL_TEST_DSN is not set in $ENV_FILE"
[[ -n "$SERVE_PASS" ]] || die "KEEL_SERVE_PASSWORD is not set in $ENV_FILE"
export KEEL_SERVE_PASSWORD="$SERVE_PASS"

# The test DSN: same server, a dedicated database name (contains e2e).
TEST_DSN="$(KEEL_DSN_VALUE="$DSN" DB_NAME="placeholder" python3 - <<'PY'
import os, re
dsn = os.environ["KEEL_DSN_VALUE"]
print(re.sub(r"/[^/?]+(\?|$)", "/placeholder\\1", dsn, count=1))
PY
)"

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

for p in "$SERVE_PORT" "$STUB_PORT"; do
  port_free "$p" || die "port $p is already in use"
done

# --- build (once) ----------------------------------------------------------
KEEL_BIN="${KEEL_BIN:-/tmp/keel-mcp-apps-e2e-$STAMP}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

WEB_DIR="$ROOT/web/dist/web/keel/Keel"
if [[ "$SKIP_WEB_BUILD" != 1 || ! -f "$WEB_DIR/index.html" ]]; then
  log "building the web target (nsh package)"
  ( cd "$ROOT/web" && nsh package --target web -o dist/web/keel ) > "$EVIDENCE/web-build.log" 2>&1
fi
[[ -f "$WEB_DIR/index.html" ]] || die "web build has no index.html at $WEB_DIR"

# --- per-run lifecycle -----------------------------------------------------
SERVE_PID=""
STUB_PID=""
DB=""
RUN_DIR=""
CLEANED=1

cleanup_current() {
  [[ "$CLEANED" == 1 ]] && return 0
  CLEANED=1
  if [[ -n "$SERVE_PID" ]]; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
  fi
  if [[ -n "$STUB_PID" ]]; then
    kill "$STUB_PID" 2>/dev/null || true
    wait "$STUB_PID" 2>/dev/null || true
  fi
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
    rm -f "$RUN_DIR/cookies.txt" "$RUN_DIR/login-body.json"
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
      for p in "$SERVE_PORT" "$STUB_PORT"; do
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
  DB=""
  RUN_DIR=""
}
trap 'rc=$?; cleanup_current; trap - EXIT; exit $rc' EXIT
trap 'exit 130' INT TERM

api() { # METHOD PATH [BODY] [JAR] → status on stdout, body in last-body.json
  local method="$1" path="$2" body="${3:-}" jar="${4:-}"
  local base="http://127.0.0.1:$SERVE_PORT"
  local -a args=(-sS)
  [[ -n "$jar" ]] && args+=(-b "$jar")
  if [[ -n "$body" ]]; then
    curl "${args[@]}" -X "$method" "$base$path" -H 'Content-Type: application/json' \
      --data-binary "$body" -o "$RUN_DIR/last-body.json" -w '%{http_code}'
  else
    curl "${args[@]}" -X "$method" "$base$path" -o "$RUN_DIR/last-body.json" -w '%{http_code}'
  fi
}

last_json() { python3 -c 'import json,sys;print(json.dumps(json.load(open(sys.argv[1])), ensure_ascii=False))' "$RUN_DIR/last-body.json"; }

run_once() { # INDEX
  local index="$1"
  RUN_DIR="$EVIDENCE/run-$index"
  mkdir -p "$RUN_DIR/screenshots"
  DB="keel_mcp_apps_e2e_${STAMP}_r${index}"
  DB="${DB//-/_}"
  CLEANED=0
  log "run $index: database $DB, evidence $RUN_DIR"
  for p in "$SERVE_PORT" "$STUB_PORT"; do
    port_free "$p" || die "port $p is still in use before run $index"
  done

  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null
  docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE \"$DB\"" >/dev/null

  # MCP fixture config: a real stdio child with the fixture log path in env.
  cat > "$RUN_DIR/mcp.json" <<JSON
{
  "mcpServers": {
    "e2e": {
      "command": "node",
      "args": ["$ROOT/scripts/mcp-apps-e2e/mcp-server.mjs"],
      "env": {"MCP_E2E_LOG": "$RUN_DIR/mcp-calls.jsonl"},
      "description": "P6A1 browser fixture (weather panel)"
    }
  }
}
JSON

  # The recording stub provider: first request asks the aggregate mcp tool to
  # call render_weather, the tool-result round answers with text.
  cat > "$RUN_DIR/models-config.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "kite-e2e",
      "name": "MCP Apps E2E (recording stub)",
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
  python3 "$ROOT/scripts/mcp-apps-e2e/stub-provider.py" "$STUB_PORT" "$RUN_DIR/wire-requests.jsonl" \
    > "$RUN_DIR/stub-provider.log" 2>&1 &
  STUB_PID=$!
  wait_port "$STUB_PORT" "$STUB_PID" "stub provider"

  TEST_DSN="$(dsn_for_db "$DB")"
  local tenant session
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  session="$(uuidgen | tr '[:upper:]' '[:lower:]')"

  KITE_E2E_KEY="kite-e2e-not-a-credential" \
  KEEL_DSN="$TEST_DSN" KEEL_TEST_DSN="$TEST_DSN" KEEL_MODELS_CONFIG="$RUN_DIR/models-config.json" \
    "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$tenant" \
      --web-dir "$WEB_DIR" --mcp "$RUN_DIR/mcp.json" > "$RUN_DIR/serve.log" 2>&1 &
  SERVE_PID=$!
  for _ in $(seq 1 150); do
    if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
    kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $RUN_DIR/serve.log)"
    sleep 0.1
  done
  curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

  local jar="$RUN_DIR/cookies.txt"
  python3 - "$RUN_DIR/login-body.json" <<'PY'
import json, os, sys
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump({"password": os.environ["KEEL_SERVE_PASSWORD"]}, handle)
PY
  curl -sS -c "$jar" -X POST "http://127.0.0.1:$SERVE_PORT/v1/auth/login" \
    -H 'Content-Type: application/json' --data-binary "@$RUN_DIR/login-body.json" > "$RUN_DIR/login.json"
  rm -f "$RUN_DIR/login-body.json"
  grep -q '"authenticated":true' "$RUN_DIR/login.json" || die "login failed (see login.json)"

  local code
  code="$(api GET /v1/capabilities "" "$jar")"
  [[ "$code" == "200" ]] || die "capabilities: HTTP $code"
  cp "$RUN_DIR/last-body.json" "$RUN_DIR/capabilities.json"
  python3 - "$RUN_DIR/capabilities.json" <<'PY'
import json, sys
features = json.load(open(sys.argv[1])).get("features") or {}
for key in ("mcp.apps", "mcp.apps.tools", "mcp.apps.resources"):
    assert features.get(key) is True, (key, features)
print("capabilities:", {k: features.get(k) for k in ("mcp.apps", "mcp.apps.tools", "mcp.apps.resources")})
PY

  code="$(api POST /v1/sessions "$(python3 -c 'import json,sys;print(json.dumps({"sessionId":sys.argv[1]}))' "$session")" "$jar")"
  [[ "$code" == "201" ]] || die "create session: HTTP $code $(last_json)"

  local input_body
  input_body="$(python3 - "$session" <<'PY'
import json, sys
print(json.dumps({
    "text": "请展示上海天气面板",
    "requestId": "mcp-apps-e2e-turn-1",
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
  python3 - "$RUN_DIR/events.json" <<'PY'
import json, re, sys
body = json.load(open(sys.argv[1]))
events = body.get("events", body if isinstance(body, list) else [])
results = [event for event in events if event.get("kind") == "tool.result"]
assert len(results) == 1, "expected exactly one tool.result, got %d" % len(results)
payload = results[0].get("payload") or {}
assert payload.get("toolName") == "mcp", payload.get("toolName")
details = payload.get("details") or {}
app = details.get("app") or {}
assert app.get("resourceUri") == "ui://weather/dashboard", app
assert re.match(r"^mcpapp_[a-f0-9]{16}$", app.get("appId") or ""), app
assert "model" in (app.get("visibility") or []) and "app" in (app.get("visibility") or []), app
print("tool.result app binding:", app)
PY

  api GET /v1/mcp/apps "" "$jar" >/dev/null
  cp "$RUN_DIR/last-body.json" "$RUN_DIR/catalogue.json"
  python3 - "$RUN_DIR/catalogue.json" <<'PY'
import json, re, sys
body = json.load(open(sys.argv[1]))
apps = body.get("apps") or []
assert len(apps) == 1, "expected one published app, got %d" % len(apps)
app = apps[0]
assert app.get("server") == "e2e", app
assert app.get("resourceUri") == "ui://weather/dashboard", app
assert re.match(r"^mcpapp_[a-f0-9]{16}$", app.get("appId") or ""), app
assert re.match(r"^[a-f0-9]{64}$", app.get("hash") or ""), app
assert app.get("documentUrl") == "/v1/mcp/apps/%s/view?v=%s" % (app["appId"], app["hash"]), app
print("catalogue:", {k: app.get(k) for k in ("appId", "hash", "documentUrl")})
PY

  # --- the real browser ----------------------------------------------------
  local browser_rc=0
  set +e
  E2E_BASE="http://127.0.0.1:$SERVE_PORT" \
  E2E_EVIDENCE="$RUN_DIR" \
  E2E_RUN="run-$index" \
  E2E_MCP_LOG="$RUN_DIR/mcp-calls.jsonl" \
  E2E_WIRE_LOG="$RUN_DIR/wire-requests.jsonl" \
  KEEL_SERVE_PASSWORD="$SERVE_PASS" \
    node "$ROOT/scripts/mcp-apps-e2e/browser.mjs" > "$RUN_DIR/browser.log" 2>&1
  browser_rc=$?
  set -e
  if [[ "$browser_rc" -ne 0 ]]; then
    log "run $index browser failed (rc=$browser_rc); see $RUN_DIR/report.html"
    cleanup_current
    exit "$browser_rc"
  fi
  cp "$RUN_DIR/report.html" "$EVIDENCE/report.html"
  cp "$RUN_DIR/report.json" "$EVIDENCE/report.json"

  # Hygiene: the evidence must not contain a DSN, the passphrase or an
  # Authorization value. (The cookie jar and login body are already removed.)
  if grep -rqF "postgres://" "$RUN_DIR" 2>/dev/null; then
    die "evidence contains a DSN (run $index)"
  fi
  if grep -rqF "postgresql://" "$RUN_DIR" 2>/dev/null; then
    die "evidence contains a DSN (run $index)"
  fi
  if grep -rqF "$SERVE_PASS" "$RUN_DIR" 2>/dev/null; then
    die "evidence contains the serve passphrase (run $index)"
  fi

  cleanup_current
  log "run $index: PASS"
}

for index in $(seq 1 "$RUNS"); do
  run_once "$index"
done

# --- cross-run summary -----------------------------------------------------
python3 - "$EVIDENCE" "$RUNS" <<'PY'
import json, os, sys
root, count = sys.argv[1], int(sys.argv[2])
runs = []
for index in range(1, count + 1):
    path = os.path.join(root, "run-%d" % index, "report.json")
    report = json.load(open(path))
    assertions = {name: value.get("ok") for name, value in (report.get("assertions") or {}).items()}
    runs.append({
        "run": index,
        "pass": bool(report.get("pass")),
        "error": report.get("error"),
        "assertions": assertions,
        "mcpMethods": report.get("mcp", {}).get("methods", {}),
        "toolCalls": sorted(call.get("name") for call in report.get("mcp", {}).get("toolCalls", [])),
        "sandbox": next((value.get("detail", {}).get("sandbox") for key, value in (report.get("assertions") or {}).items() if key == "sandbox-allow-scripts"), None),
        "csp": (report.get("documentHeaders") or {}).get("csp"),
        "hostProtocol": ((report.get("app") or {}).get("host") or {}).get("protocolVersion"),
        "fullscreenNewInstance": (report.get("fullscreen") or {}).get("newInstance"),
    })
all_pass = all(run["pass"] for run in runs)
# Consistency is about the asserted facts, not wall-clock timestamps/counts.
def fact(run):
    return {
        "assertions": run["assertions"],
        "toolCalls": run["toolCalls"],
        "sandbox": run["sandbox"],
        "csp": run["csp"],
        "hostProtocol": run["hostProtocol"],
        "fullscreenNewInstance": run["fullscreenNewInstance"],
    }
consistent = len({json.dumps(fact(run), sort_keys=True) for run in runs}) == 1 if runs else False
summary = {
    "lane": "P6A1",
    "runs": runs,
    "allPass": all_pass,
    "consistent": consistent,
}
with open(os.path.join(root, "summary.json"), "w", encoding="utf-8") as handle:
    json.dump(summary, handle, ensure_ascii=False, indent=2)
print(json.dumps({"allPass": all_pass, "consistent": consistent, "runs": [
    {"run": r["run"], "pass": r["pass"]} for r in runs]}, ensure_ascii=False))
if not all_pass or not consistent:
    sys.exit(1)
PY

log "PASS — evidence in $EVIDENCE (report.html is the latest run; summary.json compares both)"
