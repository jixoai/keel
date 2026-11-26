#!/usr/bin/env bash
# Isolated real-service E2E stack for change keel-creation (tasks 4.1/4.3).
#
# What this starts (nothing is shared with a developer deployment and no Docker
# is used):
#   * a throwaway PostgreSQL cluster under the run directory, created and
#     stopped by `keel db` (the system's PostgreSQL 18 tools);
#   * one fresh tenant UUID, one `keel serve --auth required` on a free port in
#     13600-13799, and the production web build from `scripts/package-web.sh`;
#   * a 0600 secret directory (bootstrap token, cookie key, account passwords)
#     that never reaches stdout, steps.json or the report.
#
# The browser lane is `e2e.mjs` (playwright-core + the machine's Chrome). It
# drives the real login form, the real session/approval flow through a
# deterministic scripted model, `/plugins`, the plugin page and the chunk
# render, then writes `steps.json`; `scripts/keel-identity-e2e/report-gen.mjs`
# turns that into one self-contained report.html.
#
# Usage:
#   scripts/keel-creation-e2e/run.sh
#   KEEL_E2E_SKIP_BUILD=1 KEEL_E2E_BIN=/tmp/keel KEEL_E2E_WEB_DIR=/tmp/web/Keel \
#     scripts/keel-creation-e2e/run.sh
#   KEEL_E2E_KEEP_STACK=1 ...        # keep the server/PG/secret dir for debug
#
# Requirements: go, node, python3, nsh (unless the build is supplied), the
# PostgreSQL 18 server tools (initdb/pg_ctl), Google Chrome, playwright-core.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAIN="$(cd "$ROOT/.." && pwd)"
STAMP="$(date +%F)"
RUN_DIR="${KEEL_E2E_RUN_DIR:-$MAIN/.agents/reports/keel/creation-e2e-$STAMP}"
SECRETS="$RUN_DIR/secrets"
LOG="$RUN_DIR/serve.log"
KEEP_STACK="${KEEL_E2E_KEEP_STACK:-0}"
SKIP_BUILD="${KEEL_E2E_SKIP_BUILD:-0}"
SERVER_PID=""
MODEL_PID=""
PG_STARTED=0
DB_NAME="keel_creation_e2e_$(date +%s)"
TENANT="$(python3 -c 'import uuid; print(uuid.uuid4())')"
STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

say() { printf '%s\n' "$*"; }
fail() { say "keel-creation-e2e: $*" >&2; exit 1; }

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  local port_free="not-checked" pg_stopped="not-checked" server_gone="not-checked" model_gone="not-checked"
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ -n "$SERVER_PID" ]]; then
    if kill -0 "$SERVER_PID" 2>/dev/null; then server_gone="no"; else server_gone="yes"; fi
  fi
  if [[ -n "$MODEL_PID" ]] && kill -0 "$MODEL_PID" 2>/dev/null; then
    kill "$MODEL_PID" 2>/dev/null || true
    for _ in $(seq 1 30); do kill -0 "$MODEL_PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$MODEL_PID" 2>/dev/null || true
  fi
  if [[ -n "$MODEL_PID" ]]; then
    if kill -0 "$MODEL_PID" 2>/dev/null; then model_gone="no"; else model_gone="yes"; fi
  fi
  if command -v lsof >/dev/null 2>&1 && [[ -n "${PORT:-}" ]]; then
    if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then port_free="no"; else port_free="yes"; fi
  fi
  if [[ "$PG_STARTED" == "1" ]]; then
    KEEL_HOME="$RUN_DIR/home" "${BIN:-$RUN_DIR/keel}" db stop --dir "$RUN_DIR/pg" >/dev/null 2>&1 || true
    if [[ -f "$RUN_DIR/pg/postmaster.pid" ]] && kill -0 "$(head -1 "$RUN_DIR/pg/postmaster.pid" 2>/dev/null)" 2>/dev/null; then
      pg_stopped="no"
    else
      pg_stopped="yes"
    fi
  fi
  if [[ "$KEEP_STACK" == "1" ]]; then
    say "kept stack: db=$DB_NAME port=${PORT:-} pid=${SERVER_PID:-} dir=$RUN_DIR"
  else
    rm -rf "$SECRETS" "$RUN_DIR/dsn" "$RUN_DIR/models.json" "$RUN_DIR/plugins" "$RUN_DIR/home" "$RUN_DIR/pg" 2>/dev/null || true
    [[ -f "$RUN_DIR/keel" ]] && rm -f "$RUN_DIR/keel"
    [[ -f "$RUN_DIR/web" ]] && rm -rf "$RUN_DIR/web"
  fi
  python3 - "$RUN_DIR" "$DB_NAME" "${PORT:-}" "${SERVER_PID:-}" "${MODEL_PORT:-}" "${MODEL_PID:-}" "$KEEP_STACK" \
    "$port_free" "$pg_stopped" "$server_gone" "$model_gone" "$STARTED_AT" "$code" <<'PY'
import json, sys, datetime
(run_dir, db, port, pid, model_port, model_pid, keep,
 port_free, pg_stopped, server_gone, model_gone, started, code) = sys.argv[1:]
payload = {
    "database": db,
    "port": port,
    "serverPid": pid,
    "kept": keep == "1",
    "scriptedModelPort": model_port,
    "scriptedModelPid": model_pid,
    "portFree": port_free,
    "postgresStopped": pg_stopped,
    "serverExited": server_gone,
    "scriptedModelExited": model_gone,
    "startedAt": started,
    "finishedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "exitCode": int(code),
}
try:
    with open(run_dir + "/cleanup.json", "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2, ensure_ascii=False)
        fh.write("\n")
except OSError:
    pass
PY
  exit "$code"
}
trap cleanup EXIT INT TERM

mkdir -p "$RUN_DIR"
chmod 700 "$RUN_DIR"
mkdir -p "$SECRETS"
chmod 700 "$SECRETS"

# ---- 1. free ports --------------------------------------------------------
if [[ -n "${KEEL_E2E_PORT:-}" ]]; then
  PORT="$KEEL_E2E_PORT"
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1 && fail "KEEL_E2E_PORT=$PORT is already listening"
else
  PORT=""
  for candidate in $(seq 13600 13799); do
    if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then PORT="$candidate"; break; fi
  done
  [[ -n "$PORT" ]] || fail "no free port in 13600-13799"
fi
MODEL_PORT="${KEEL_E2E_SCRIPTED_PORT:-}"
if [[ -z "$MODEL_PORT" ]]; then
  for candidate in $(seq 13790 13799) $(seq 13600 13789); do
    if [[ "$candidate" != "$PORT" ]] && ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
      MODEL_PORT="$candidate"; break
    fi
  done
fi
[[ -n "$MODEL_PORT" ]] || fail "no free scripted-model port"
PG_PORT=""
for candidate in $(seq 13741 13789); do
  if [[ "$candidate" != "$PORT" && "$candidate" != "$MODEL_PORT" ]] && ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
    PG_PORT="$candidate"; break
  fi
done
[[ -n "$PG_PORT" ]] || fail "no free PostgreSQL port"

# ---- 2. build artifacts ---------------------------------------------------
BIN="${KEEL_E2E_BIN:-$RUN_DIR/keel}"
WEB_DIR="${KEEL_E2E_WEB_DIR:-}"
if [[ "$SKIP_BUILD" != "1" ]]; then
  say "building keel -> $BIN"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/keel ) || fail "go build failed"
  if [[ -z "$WEB_DIR" ]]; then
    WEB_DIR="$RUN_DIR/web"
    say "packaging web -> $WEB_DIR (this takes a few minutes)"
    "$ROOT/scripts/package-web.sh" "$WEB_DIR" || fail "web packaging failed"
  fi
fi
[[ -x "$BIN" ]] || fail "missing keel binary: $BIN"
[[ -n "$WEB_DIR" && ( -f "$WEB_DIR/Keel/index.html" || -f "$WEB_DIR/index.html" ) ]] || \
  fail "missing web build: $WEB_DIR (set KEEL_E2E_WEB_DIR or KEEL_E2E_SKIP_BUILD=0)"

# ---- 3. secrets and model config -----------------------------------------
python3 - "$SECRETS" <<'PY'
import os, secrets, sys
d = sys.argv[1]
def write(name, value):
    path = os.path.join(d, name)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(value)
    os.chmod(path, 0o600)
write("bootstrap.token", "ksetup_" + secrets.token_urlsafe(32))
write("secret.key", secrets.token_hex(32))
write("admin.initial", secrets.token_urlsafe(18))
write("admin.password", secrets.token_urlsafe(18))
write("viewer.password", secrets.token_urlsafe(18))
PY
MODELS_CONFIG="$RUN_DIR/models.json"
python3 - "$MODEL_PORT" "$MODELS_CONFIG" <<'PY'
import json, sys
port, out = sys.argv[1], sys.argv[2]
document = {
    "schemaVersion": 1,
    "providers": [{
        "id": "e2e-scripted",
        "name": "E2E Scripted",
        "api": {"type": "openai-chat-completions", "baseUrl": f"http://127.0.0.1:{port}/v1"},
        "access": {"type": "api-key", "env": "KEEL_E2E_MODEL_KEY"},
        "modelIds": ["e2e-creation"],
        "visibility": "visible",
    }],
    "modelRules": [{
        "match": "*",
        "config": {
            "enabled": True,
            "properties": {
                "contextWindow": 64000,
                "inputFormat": {"text": True, "image": False, "video": False, "audio": False, "pdf": False},
                "outputFormat": {"text": True},
                "supportsToolCall": True,
                "supportsJsonSchemaOutput": False,
                "supportsNativeWebSearch": False,
            },
            "options": {
                "reasoningLevel": {"values": ["off"], "map": {"off": {}}},
                "maxOutputTokens": {"max": 2048, "map": {}},
            },
        },
    }],
    "selection": {"providerId": "e2e-scripted", "modelId": "e2e-creation"},
}
with open(out, "w", encoding="utf-8") as fh:
    json.dump(document, fh, indent=2)
    fh.write("\n")
PY

# ---- 4. disposable PostgreSQL --------------------------------------------
say "starting a throwaway PostgreSQL cluster on 127.0.0.1:$PG_PORT"
KEEL_HOME="$RUN_DIR/home" "$BIN" db start \
  --dir "$RUN_DIR/pg" --port "$PG_PORT" --database "$DB_NAME" --user keel \
  >"$RUN_DIR/db.out" 2>"$RUN_DIR/db.err" || { cat "$RUN_DIR/db.err" >&2; fail "keel db start failed"; }
PG_STARTED=1
DSN="$(KEEL_HOME="$RUN_DIR/home" "$BIN" db dsn --dir "$RUN_DIR/pg" 2>/dev/null)" || fail "keel db dsn failed"
[[ -n "$DSN" ]] || fail "empty DSN"
printf '%s\n' "$DSN" > "$RUN_DIR/dsn"
chmod 600 "$RUN_DIR/dsn"
rm -f "$RUN_DIR/db.out" "$RUN_DIR/db.err"

# ---- 5. deterministic scripted model -------------------------------------
say "starting scripted creation model on 127.0.0.1:$MODEL_PORT"
SCRIPTED_MODEL_PORT="$MODEL_PORT" \
SCRIPTED_MODEL_LOG="$RUN_DIR/scripted-model.jsonl" \
PLAYWRIGHT_CORE="${PLAYWRIGHT_CORE:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}" \
  node "$ROOT/scripts/keel-creation-e2e/scripted-model.mjs" \
  >"$RUN_DIR/scripted-model.stdout.log" 2>&1 &
MODEL_PID=$!
for _ in $(seq 1 50); do
  curl -fsS --max-time 1 "http://127.0.0.1:$MODEL_PORT/health" >/dev/null 2>&1 && break
  kill -0 "$MODEL_PID" 2>/dev/null || { tail -20 "$RUN_DIR/scripted-model.stdout.log" >&2; fail "scripted model exited"; }
  sleep 0.1
done
curl -fsS --max-time 1 "http://127.0.0.1:$MODEL_PORT/health" >/dev/null 2>&1 || fail "scripted model is not healthy"

# ---- 6. serve -------------------------------------------------------------
say "starting keel serve on 127.0.0.1:$PORT (evidence $RUN_DIR)"
KEEL_HOME="$RUN_DIR/home" \
KEEL_DSN="$DSN" \
KEEL_BOOTSTRAP_TOKEN="$(cat "$SECRETS/bootstrap.token")" \
KEEL_SECRET_KEY="$(cat "$SECRETS/secret.key")" \
KEEL_SERVE_TENANT="$TENANT" \
KEEL_MODELS_CONFIG="$MODELS_CONFIG" \
KEEL_E2E_MODEL_KEY="e2e-scripted-key" \
"$BIN" serve --addr "127.0.0.1:$PORT" --auth required --web-dir "$WEB_DIR" --plugins-root "$RUN_DIR/plugins" \
  >"$LOG" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 120); do
  curl -fsS --max-time 1 "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1 && break
  kill -0 "$SERVER_PID" 2>/dev/null || { tail -30 "$LOG" >&2; fail "server exited before readiness"; }
  sleep 0.2
done
curl -fsS --max-time 1 "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1 || fail "server is not healthy"

# ---- 7. first super admin (real login; secrets stay in 0600 files) --------
KEEL_ACCOUNT_PASSWORD="$(cat "$SECRETS/admin.initial")" \
KEEL_ACCOUNT_NEW_PASSWORD="$(cat "$SECRETS/admin.password")" \
python3 "$ROOT/scripts/lib/keel-auth.py" bootstrap \
  --base "http://127.0.0.1:$PORT" \
  --token-file "$SECRETS/bootstrap.token" \
  --username e2e-admin \
  --jar "$SECRETS/cookies.txt" \
  --csrf-file "$SECRETS/csrf.txt" > "$SECRETS/auth.json" || fail "admin bootstrap/login failed"
rm -f "$SECRETS/admin.initial" "$SECRETS/auth.json"

# ---- 8. browser lane ------------------------------------------------------
export E2E_BASE="http://127.0.0.1:$PORT"
export E2E_EVIDENCE="$RUN_DIR"
export E2E_WORKTREE="$ROOT"
export E2E_HEAD_COMMIT="$(cd "$ROOT" && git rev-parse HEAD)"
export E2E_ADMIN_USERNAME="e2e-admin"
export E2E_ADMIN_PASSWORD_FILE="$SECRETS/admin.password"
export E2E_VIEWER_PASSWORD_FILE="$SECRETS/viewer.password"
export E2E_TENANT="$TENANT"
export E2E_DSN_FILE="$RUN_DIR/dsn"
export E2E_DB="$DB_NAME"
export E2E_PG_PORT="$PG_PORT"
export E2E_PLUGIN_ID="e2e.panel"
export E2E_PLUGINS_ROOT="$RUN_DIR/plugins"
export E2E_CHROME="${E2E_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
export PLAYWRIGHT_CORE="${PLAYWRIGHT_CORE:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}"
export E2E_HEADLESS="${E2E_HEADLESS:-true}"

set +e
node "$ROOT/scripts/keel-creation-e2e/e2e.mjs"
E2E_CODE=$?
set -e

if [[ -f "$RUN_DIR/steps.json" ]]; then
  node "$ROOT/scripts/keel-identity-e2e/report-gen.mjs" "$RUN_DIR" || fail "report generation failed"
else
  say "no steps.json produced" >&2
fi

say "evidence: $RUN_DIR"
say "report: $RUN_DIR/report.html"
say "steps exit: $E2E_CODE"

exit "$E2E_CODE"
