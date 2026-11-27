#!/usr/bin/env bash
# Real-service browser E2E stack for change keel-plugin-attachment (tasks 5.4,
# 5.5 and 3.6).
#
# What this starts:
#   * a throwaway PostgreSQL cluster under the run directory (`keel db`);
#   * one deterministic scripted model (OpenAI Chat Completions streaming) that
#     records every request body under the run directory;
#   * tenant A on port A and tenant B on port B, both `keel serve --auth
#     required` against the same database and the packaged web build;
#   * a 0600 secret directory (bootstrap token, cookie key, account passwords)
#     that never reaches stdout, steps.json or the report.
#
# The browser lane is `e2e.mjs` (playwright-core + the machine's Chrome). It
# uploads real files over HTTP, sends a real input, opens the real conversation,
# installs/disables real plugin packages and checks the cross-tenant and
# permission boundaries. `scripts/keel-identity-e2e/report-gen.mjs` turns
# steps.json into a self-contained report.html.
#
# Usage:
#   scripts/keel-attachment-e2e/run.sh
#   KEEL_E2E_SKIP_BUILD=1 KEEL_E2E_BIN=/tmp/keel KEEL_E2E_WEB_DIR=/tmp/web/Keel \
#     scripts/keel-attachment-e2e/run.sh
#   KEEL_E2E_KEEP_STACK=1 ...        # keep servers/PG/secrets for debug
#
# Requirements: go, node, python3, nsh (unless a build is supplied), the
# PostgreSQL 18 server tools (initdb/pg_ctl), Google Chrome, playwright-core.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_DIR="${KEEL_E2E_RUN_DIR:-$ROOT/.agents/reports/keel/attachment-e2e-$(date +%F-%H%M%S)}"
EVIDENCE="${KEEL_E2E_EVIDENCE:-$ROOT/openspec/changes/keel-plugin-attachment/evidence/ui}"
SECRETS="$RUN_DIR/secrets"
LOG="$RUN_DIR/serve-a.log"
LOG_B="$RUN_DIR/serve-b.log"
KEEP_STACK="${KEEL_E2E_KEEP_STACK:-0}"
SKIP_BUILD="${KEEL_E2E_SKIP_BUILD:-0}"
SERVER_PID=""
SERVER_B_PID=""
MODEL_PID=""
PG_STARTED=0
DB_NAME="keel_attachment_e2e_$(date +%s)"
TENANT="$(python3 -c 'import uuid; print(uuid.uuid4())')"
TENANT_B="$(python3 -c 'import uuid; print(uuid.uuid4())')"
STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

say() { printf '%s\n' "$*"; }
fail() { say "keel-attachment-e2e: $*" >&2; exit 1; }

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  local port_free="not-checked" port_b_free="not-checked" pg_stopped="not-checked"
  local server_gone="not-checked" server_b_gone="not-checked" model_gone="not-checked"
  for pid_var in SERVER_PID SERVER_B_PID MODEL_PID; do
    local pid="${!pid_var:-}"
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
  if [[ -n "$SERVER_PID" ]]; then kill -0 "$SERVER_PID" 2>/dev/null && server_gone="no" || server_gone="yes"; fi
  if [[ -n "$SERVER_B_PID" ]]; then kill -0 "$SERVER_B_PID" 2>/dev/null && server_b_gone="no" || server_b_gone="yes"; fi
  if [[ -n "$MODEL_PID" ]]; then kill -0 "$MODEL_PID" 2>/dev/null && model_gone="no" || model_gone="yes"; fi
  if command -v lsof >/dev/null 2>&1 && [[ -n "${PORT:-}" ]]; then
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1 && port_free="no" || port_free="yes"
  fi
  if command -v lsof >/dev/null 2>&1 && [[ -n "${PORT_B:-}" ]]; then
    lsof -nP -iTCP:"$PORT_B" -sTCP:LISTEN >/dev/null 2>&1 && port_b_free="no" || port_b_free="yes"
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
    say "kept stack: db=$DB_NAME port=${PORT:-} portB=${PORT_B:-} pid=${SERVER_PID:-} dir=$RUN_DIR"
  else
    rm -rf "$SECRETS" "$RUN_DIR/dsn" "$RUN_DIR/models.json" "$RUN_DIR/plugins" "$RUN_DIR/plugins-b" \
      "$RUN_DIR/home" "$RUN_DIR/home-b" "$RUN_DIR/pg" 2>/dev/null || true
    [[ -f "$RUN_DIR/keel" ]] && rm -f "$RUN_DIR/keel"
    [[ -f "$RUN_DIR/seed-tenant" ]] && rm -f "$RUN_DIR/seed-tenant"
    [[ -d "$RUN_DIR/web" ]] && rm -rf "$RUN_DIR/web"
  fi
  python3 - "$RUN_DIR" "$DB_NAME" "${PORT:-}" "${PORT_B:-}" "${PORT_B:-}" "${SERVER_PID:-}" "${SERVER_B_PID:-}" \
    "${MODEL_PID:-}" "$KEEP_STACK" "$port_free" "$port_b_free" "$pg_stopped" "$server_gone" "$server_b_gone" \
    "$model_gone" "$STARTED_AT" "$code" <<'PY'
import json, sys, datetime
(run_dir, db, port, port_b, _unused, pid, pid_b, model_pid, keep, port_free, port_b_free,
 pg_stopped, server_gone, server_b_gone, model_gone, started, code) = sys.argv[1:]
payload = {
    "database": db,
    "port": port,
    "portB": port_b,
    "serverPid": pid,
    "serverBPid": pid_b,
    "scriptedModelPid": model_pid,
    "kept": keep == "1",
    "portFree": port_free,
    "portBFree": port_b_free,
    "postgresStopped": pg_stopped,
    "serverExited": server_gone,
    "serverBExited": server_b_gone,
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

mkdir -p "$RUN_DIR" "$EVIDENCE"
chmod 700 "$RUN_DIR"
mkdir -p "$SECRETS"
chmod 700 "$SECRETS"

# ---- 1. free ports --------------------------------------------------------
pick_port() {
  local p
  for p in $(seq "$1" "$2"); do
    if ! lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
      printf '%s' "$p"
      return 0
    fi
  done
  return 1
}
PORT="${KEEL_E2E_PORT:-$(pick_port 13600 13680)}"
PORT_B="${KEEL_E2E_PORT_B:-$(pick_port 13681 13720)}"
MODEL_PORT="${KEEL_E2E_SCRIPTED_PORT:-$(pick_port 13790 13799)}"
PG_PORT="$(pick_port 13721 13789)"
[[ -n "$PORT" && -n "$PORT_B" && -n "$MODEL_PORT" && -n "$PG_PORT" ]] || fail "no free ports in 13600-13799"
[[ "$PORT" != "$PORT_B" && "$PORT" != "$MODEL_PORT" && "$PORT_B" != "$MODEL_PORT" ]] || fail "port collision"

# ---- 2. build artifacts ---------------------------------------------------
BIN="${KEEL_E2E_BIN:-$RUN_DIR/keel}"
WEB_DIR="${KEEL_E2E_WEB_DIR:-$RUN_DIR/web}"
if [[ "$SKIP_BUILD" != "1" ]]; then
  say "building keel -> $BIN"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/keel ) || fail "go build failed"
  say "building seed-tenant"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$RUN_DIR/seed-tenant" ./scripts/keel-attachment-e2e/seed-tenant ) \
    || fail "seed-tenant build failed"
  say "packaging web -> $WEB_DIR (this takes a few minutes)"
  "$ROOT/scripts/package-web.sh" "$WEB_DIR" || fail "web packaging failed"
fi
[[ -x "$BIN" ]] || fail "missing keel binary: $BIN"
[[ -x "$RUN_DIR/seed-tenant" ]] || fail "missing seed-tenant binary"
[[ -f "$WEB_DIR/Keel/index.html" || -f "$WEB_DIR/index.html" ]] || \
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
write("reader.password", secrets.token_urlsafe(18))
write("outsider.password", secrets.token_urlsafe(18))
write("tenant-b.password", secrets.token_urlsafe(18))
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
        "modelIds": ["e2e-attachment"],
        "visibility": "visible",
    }],
    "modelRules": [{
        "match": "*",
        "config": {
            "enabled": True,
            "properties": {
                "contextWindow": 64000,
                # The decisive matrix for this lane: PNG travels natively,
                # PDF does not and must produce the text fallback.
                "inputFormat": {"text": True, "image": True, "video": False, "audio": False, "pdf": False},
                "outputFormat": {"text": True},
                "supportsToolCall": False,
                "supportsJsonSchemaOutput": False,
                "supportsNativeWebSearch": False,
            },
            "options": {
                "reasoningLevel": {"values": ["off"], "map": {"off": {}}},
                "maxOutputTokens": {"max": 512, "map": {}},
            },
        },
    }],
    "selection": {"providerId": "e2e-scripted", "modelId": "e2e-attachment"},
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
say "starting scripted model on 127.0.0.1:$MODEL_PORT"
SCRIPTED_MODEL_PORT="$MODEL_PORT" \
SCRIPTED_MODEL_LOG="$RUN_DIR/scripted-model.jsonl" \
  node "$ROOT/scripts/keel-attachment-e2e/scripted-model.mjs" \
  >"$RUN_DIR/scripted-model.stdout.log" 2>&1 &
MODEL_PID=$!
for _ in $(seq 1 50); do
  curl -fsS --max-time 1 "http://127.0.0.1:$MODEL_PORT/health" >/dev/null 2>&1 && break
  kill -0 "$MODEL_PID" 2>/dev/null || { tail -20 "$RUN_DIR/scripted-model.stdout.log" >&2; fail "scripted model exited"; }
  sleep 0.1
done
curl -fsS --max-time 1 "http://127.0.0.1:$MODEL_PORT/health" >/dev/null 2>&1 || fail "scripted model is not healthy"

# ---- 6. serve tenant A ----------------------------------------------------
say "starting keel serve (tenant A) on 127.0.0.1:$PORT (evidence $EVIDENCE)"
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
  kill -0 "$SERVER_PID" 2>/dev/null || { tail -40 "$LOG" >&2; fail "server A exited before readiness"; }
  sleep 0.2
done
curl -fsS --max-time 1 "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1 || fail "server A is not healthy"

# ---- 7. first super admin (real login; secrets stay in 0600 files) -------
# Before tenant B's account: the bootstrap token is refused once any super
# admin exists, and the seed creates one in tenant B.
KEEL_ACCOUNT_PASSWORD="$(cat "$SECRETS/admin.initial")" \
KEEL_ACCOUNT_NEW_PASSWORD="$(cat "$SECRETS/admin.password")" \
python3 "$ROOT/scripts/lib/keel-auth.py" bootstrap \
  --base "http://127.0.0.1:$PORT" \
  --token-file "$SECRETS/bootstrap.token" \
  --username e2e-admin \
  --jar "$SECRETS/cookies.txt" \
  --csrf-file "$SECRETS/csrf.txt" > "$SECRETS/auth.json" || fail "admin bootstrap/login failed"
rm -f "$SECRETS/admin.initial" "$SECRETS/auth.json"

# ---- 8. tenant B account and server -------------------------------------
say "seeding tenant B ($TENANT_B)"
KEEL_DSN="$DSN" "$RUN_DIR/seed-tenant" \
  -tenant "$TENANT_B" -username tenant-b-admin -password "$(cat "$SECRETS/tenant-b.password")" \
  || fail "seed-tenant failed"

say "starting keel serve (tenant B) on 127.0.0.1:$PORT_B"
KEEL_HOME="$RUN_DIR/home-b" \
KEEL_DSN="$DSN" \
KEEL_SECRET_KEY="$(cat "$SECRETS/secret.key")" \
KEEL_SERVE_TENANT="$TENANT_B" \
KEEL_MODELS_CONFIG="$MODELS_CONFIG" \
KEEL_E2E_MODEL_KEY="e2e-scripted-key" \
"$BIN" serve --addr "127.0.0.1:$PORT_B" --auth required --web-dir "$WEB_DIR" --plugins-root "$RUN_DIR/plugins-b" \
  >"$LOG_B" 2>&1 &
SERVER_B_PID=$!
for _ in $(seq 1 120); do
  curl -fsS --max-time 1 "http://127.0.0.1:$PORT_B/v1/health" >/dev/null 2>&1 && break
  kill -0 "$SERVER_B_PID" 2>/dev/null || { tail -40 "$LOG_B" >&2; fail "server B exited before readiness"; }
  sleep 0.2
done
curl -fsS --max-time 1 "http://127.0.0.1:$PORT_B/v1/health" >/dev/null 2>&1 || fail "server B is not healthy"

# ---- 9. browser lane ------------------------------------------------------
export E2E_BASE="http://127.0.0.1:$PORT"
export E2E_BASE_B="http://127.0.0.1:$PORT_B"
export E2E_EVIDENCE="$EVIDENCE"
export E2E_RUN_DIR="$RUN_DIR"
export E2E_WORKTREE="$ROOT"
export E2E_HEAD_COMMIT="$(cd "$ROOT" && git rev-parse HEAD)"
export E2E_COVERAGE_BASE="${E2E_COVERAGE_BASE:-8916393}"
export E2E_ADMIN_USERNAME="e2e-admin"
export E2E_ADMIN_PASSWORD_FILE="$SECRETS/admin.password"
export E2E_READER_PASSWORD_FILE="$SECRETS/reader.password"
export E2E_OUTSIDER_PASSWORD_FILE="$SECRETS/outsider.password"
export E2E_TENANT_B_PASSWORD_FILE="$SECRETS/tenant-b.password"
export E2E_TENANT="$TENANT"
export E2E_TENANT_B="$TENANT_B"
export E2E_DSN_FILE="$RUN_DIR/dsn"
export E2E_MODEL_LOG="$RUN_DIR/scripted-model.jsonl"
export E2E_PLUGINS_ROOT="$RUN_DIR/e2e-plugins"
export E2E_CHROME="${E2E_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
export PLAYWRIGHT_CORE="${PLAYWRIGHT_CORE:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}"
export E2E_HEADLESS="${E2E_HEADLESS:-true}"

set +e
node "$ROOT/scripts/keel-attachment-e2e/e2e.mjs"
E2E_CODE=$?
set -e

if [[ -f "$EVIDENCE/steps.json" ]]; then
  node "$ROOT/scripts/keel-identity-e2e/report-gen.mjs" "$EVIDENCE" || fail "report generation failed"
else
  say "no steps.json produced"
fi
exit "$E2E_CODE"
