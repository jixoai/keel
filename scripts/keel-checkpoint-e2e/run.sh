#!/usr/bin/env bash
# Isolated real-service E2E for change keel-checkpoint (checkpoint policy UI +
# automatic/manual compaction + kill -9 recovery).
#
# What this starts (nothing is shared with a developer deployment):
#   * a throwaway PostgreSQL database created with the local `createdb` and
#     dropped by this script (no Docker);
#   * one `keel serve --auth local-only` on a free port in 13600-13799, built
#     from this worktree;
#   * the production web build from `nsh package --target web`;
#   * a deterministic loopback Chat Completions provider
#     (`fake-provider.mjs`) whose usage matches the prompt size, so the
#     automatic compaction threshold is reachable;
#   * a 0600 secret-key file used only to keep the browser session valid across
#     the kill -9 restart. It is removed by cleanup.
#
# The browser lane (playwright-core + the machine's Chrome) runs in `e2e.mjs`;
# `report.mjs` turns steps.json + screenshots into report.html.
#
# Usage:
#   scripts/keel-checkpoint-e2e/run.sh
#   KEEL_E2E_SKIP_BUILD=1 scripts/keel-checkpoint-e2e/run.sh   # reuse bin/web
#   KEEL_E2E_KEEP_STACK=1 ...                                  # keep DB/server
#
# Requirements: go, nsh, node, psql/createdb/dropdb, Chrome, playwright-core.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAIN="$(cd "$ROOT/../.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
BASE_DIR="${KEEL_E2E_REPORT_DIR:-$MAIN/.agents/reports/keel/checkpoint-e2e-$(date +%Y-%m-%d)}"
# Keep one canonical date directory: a previous interrupted scratch directory
# is preserved next to it instead of being deleted silently.
if [[ -e "$BASE_DIR" && ! -f "$BASE_DIR/report.html" ]]; then
  mv "$BASE_DIR" "$BASE_DIR.previous-$STAMP"
fi
RUN_DIR="$BASE_DIR"
CACHE="${KEEL_E2E_CACHE:-/tmp/keel-checkpoint-e2e-build-$STAMP}"
SECRETS="$RUN_DIR/secrets"
RUNTIME="$RUN_DIR/runtime"
SHOTS="$RUN_DIR/shots"
DB="keel_checkpoint_e2e_$(date +%s)"
TENANT="00000000-0000-0000-0000-00000000a001"
PG_USER="${KEEL_E2E_PG_USER:-$(whoami)}"
DSN_BASE="${KEEL_E2E_DSN_BASE:-postgres://$PG_USER@127.0.0.1:5432}"
KEEP_STACK="${KEEL_E2E_KEEP_STACK:-0}"
SKIP_BUILD="${KEEL_E2E_SKIP_BUILD:-0}"
SERVER_PID=""
PROVIDER_PID=""
DB_CREATED=0

say() { printf '%s\n' "$*"; }
fail() { say "keel-checkpoint-e2e: $*" >&2; exit 1; }

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  if [[ -f "$RUNTIME/server.pid" ]]; then
    local pid
    pid="$(cat "$RUNTIME/server.pid" 2>/dev/null || true)"
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  fi
  if [[ -n "$PROVIDER_PID" ]] && kill -0 "$PROVIDER_PID" 2>/dev/null; then
    kill -9 "$PROVIDER_PID" 2>/dev/null || true
  fi
  if [[ "$KEEP_STACK" == "1" ]]; then
    say "kept stack: db=$DB dir=$RUN_DIR"
  else
    if [[ "$DB_CREATED" == "1" ]]; then
      dropdb --force --if-exists "$DB" >/dev/null 2>&1 || true
    fi
    rm -rf "$SECRETS" "$RUN_DIR/dsn" 2>/dev/null || true
  fi
  python3 - "$RUN_DIR" "$DB" "${SERVER_PID:-}" "$KEEP_STACK" "$code" <<'PY' 2>/dev/null || true
import datetime, json, sys
run_dir, db, pid, keep, code = sys.argv[1:]
payload = {
    "database": db,
    "kept": keep == "1",
    "serverPid": pid,
    "finishedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "exitCode": int(code),
}
with open(run_dir + "/cleanup.json", "w", encoding="utf-8") as fh:
    json.dump(payload, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
PY
  exit "$code"
}
trap cleanup EXIT INT TERM

command -v go >/dev/null || fail "go is required"
command -v node >/dev/null || fail "node is required"
command -v psql >/dev/null || fail "psql (PostgreSQL client) is required"
command -v createdb >/dev/null || fail "createdb is required"
mkdir -p "$SECRETS" "$RUNTIME" "$SHOTS" "$CACHE"
chmod 700 "$SECRETS" "$RUN_DIR"

# ---- ports: first free in the project's 13600-13799 window -----------------
pick_port() {
  local exclude="${1:-}"
  local candidate
  for candidate in $(seq 13757 13799) $(seq 13600 13756); do
    [[ "$candidate" == "$exclude" ]] && continue
    if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}
PORT="${KEEL_E2E_PORT:-$(pick_port)}"
[[ -n "$PORT" ]] || fail "no free serve port in 13600-13799"
MODEL_PORT="${KEEL_E2E_MODEL_PORT:-$(pick_port "$PORT")}"
[[ -n "$MODEL_PORT" ]] || fail "no free model port in 13600-13799"

# ---- build binary and web --------------------------------------------------
BIN="${KEEL_E2E_BIN:-$CACHE/keel}"
WEB_DIR="${KEEL_E2E_WEB_DIR:-$CACHE/web}"
if [[ "$SKIP_BUILD" != "1" ]]; then
  say "building keel -> $BIN"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/keel ) || fail "go build failed"
  say "packaging web -> $WEB_DIR (this takes a few minutes)"
  ( cd "$ROOT/web" && nsh package --target web -o "$WEB_DIR" ) || fail "nsh package failed"
fi
[[ -x "$BIN" ]] || fail "missing keel binary: $BIN"
SITE_ROOT="$WEB_DIR/Keel"
[[ -f "$SITE_ROOT/index.html" ]] || SITE_ROOT="$WEB_DIR"
[[ -f "$SITE_ROOT/index.html" ]] || fail "missing web build: $WEB_DIR (set KEEL_E2E_SKIP_BUILD=0)"

# ---- database and secrets --------------------------------------------------
say "creating database $DB"
dropdb --force --if-exists "$DB" >/dev/null 2>&1 || true
createdb "$DB" || fail "createdb failed"
DB_CREATED=1
DSN="$DSN_BASE/$DB?sslmode=disable"
printf '%s' "$DSN" > "$RUN_DIR/dsn"
chmod 600 "$RUN_DIR/dsn"
python3 - "$SECRETS" <<'PY'
import os, secrets, sys
d = sys.argv[1]
path = os.path.join(d, "secret.key")
with open(path, "w", encoding="utf-8") as fh:
    fh.write(secrets.token_hex(32))
os.chmod(path, 0o600)
PY

cat > "$RUNTIME/models.json" <<JSON
{
  "providers": [{
    "id": "e2e",
    "name": "E2E Provider",
    "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$MODEL_PORT/v1"},
    "access": {"type": "api-key", "env": "E2E_PROVIDER_KEY"},
    "modelIds": ["e2e-model"]
  }],
  "selection": {"providerId": "e2e", "modelId": "e2e-model"},
  "modelRules": [{
    "match": "e2e-model",
    "config": {"properties": {
      "contextWindow": 30000,
      "inputFormat": {"text": true},
      "outputFormat": {"text": true},
      "supportsToolCall": true
    }}
  }]
}
JSON

cat > "$RUNTIME/config.toml" <<'TOML'
version = 1

[compaction]
enabled = true
thresholdTokens = 1000
keepRecentTokens = 20000
maxCheckpoints = 3
ttlHours = 168
manualCompact = true
TOML

# ---- deterministic provider ------------------------------------------------
say "starting fake provider on 127.0.0.1:$MODEL_PORT"
E2E_MODEL_PORT="$MODEL_PORT" node "$ROOT/scripts/keel-checkpoint-e2e/fake-provider.mjs" "$MODEL_PORT" \
  >"$RUNTIME/fake-provider.log" 2>&1 &
PROVIDER_PID=$!
READY=0
for _ in $(seq 1 50); do
  if curl -fsS --max-time 1 "http://127.0.0.1:$MODEL_PORT/v1/models" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 0.1
done
[[ "$READY" == "1" ]] || fail "fake provider did not become healthy"

# ---- browser lane ----------------------------------------------------------
export E2E_BASE="http://127.0.0.1:$PORT"
export E2E_EVIDENCE="$RUN_DIR"
export E2E_HEAD_COMMIT="$(cd "$ROOT" && git rev-parse HEAD)"
export E2E_KEEL_BIN="$BIN"
export E2E_WEB_DIR="$SITE_ROOT"
export E2E_DSN_FILE="$RUN_DIR/dsn"
export E2E_DB="$DB"
export E2E_TENANT="$TENANT"
export E2E_PORT="$PORT"
export E2E_MODEL_PORT="$MODEL_PORT"
export E2E_MODELS_CONFIG="$RUNTIME/models.json"
export E2E_CONFIG_FILE="$RUNTIME/config.toml"
export E2E_HOME="$RUNTIME/home"
export E2E_CAS_DIR="$RUNTIME/cas"
export E2E_SECRET_FILE="$SECRETS/secret.key"
export E2E_CHROME="${E2E_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
export PLAYWRIGHT_CORE="${PLAYWRIGHT_CORE:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}"
export E2E_HEADLESS="${E2E_HEADLESS:-true}"
export E2E_PUBLIC_URL="${E2E_PUBLIC_URL:-}"

[[ -f "$E2E_CHROME" ]] || fail "Chrome not found: $E2E_CHROME"
[[ -f "$PLAYWRIGHT_CORE" ]] || fail "playwright-core not found: $PLAYWRIGHT_CORE"

set +e
node "$ROOT/scripts/keel-checkpoint-e2e/e2e.mjs"
E2E_CODE=$?
set -e

# Terminal-host evidence for the same section (fixture transport): the TUI
# renders the effective policy as text and can narrow/reset it.
TUI_LOG="$RUN_DIR/tui-test.log"
TUI_CODE=0
if [[ "${KEEL_E2E_SKIP_TUI:-0}" != "1" ]] && command -v nsh >/dev/null 2>&1; then
  say "running TUI checkpoint test"
  ( cd "$ROOT/web" && nsh test --target tui --filter '检查点策略' ) >"$TUI_LOG" 2>&1
  TUI_CODE=$?
  say "TUI test exit: $TUI_CODE"
fi

if [[ -f "$RUN_DIR/steps.json" ]]; then
  node "$ROOT/scripts/keel-checkpoint-e2e/report.mjs" "$RUN_DIR" || fail "report generation failed"
else
  say "no steps.json produced" >&2
fi
if [[ "$E2E_CODE" == "0" && "$TUI_CODE" != "0" ]]; then
  E2E_CODE=1
fi
say "evidence: $RUN_DIR"
say "steps exit: $E2E_CODE"
exit "$E2E_CODE"
