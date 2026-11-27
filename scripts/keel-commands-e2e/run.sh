#!/usr/bin/env bash
# Real-service E2E for the chat-commands change (keel-plugin-commands 5.4).
#
# What this starts (nothing is shared with a developer deployment):
#   * a throwaway PostgreSQL database created with the local `createdb` and
#     dropped by this script (no Docker);
#   * one `keel serve --auth local-only` on a free port in 13600-13799, built
#     from this worktree, with the production web build;
#   * a deterministic loopback Chat Completions provider (reused from the
#     checkpoint lane) for the manual `/compact` summary;
#   * the test plugin `scripts/keel-commands-e2e/plugin` (one sidecar with
#     write/destructive commands), installed and approved through the real
#     two-phase plugin lifecycle during the run.
#
# The browser lane (playwright-core + the machine's Chrome) runs in `e2e.mjs`;
# `report.mjs` turns steps.json + screenshots into report.html.
#
# Usage:
#   scripts/keel-commands-e2e/run.sh
#   KEEL_E2E_SKIP_BUILD=1 scripts/keel-commands-e2e/run.sh   # reuse bin/web
#   KEEL_E2E_KEEP_STACK=1 ...                                # keep DB/server
#
# Requirements: go, nsh, node, psql/createdb/dropdb, Chrome, playwright-core.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
BASE_DIR="${KEEL_E2E_REPORT_DIR:-$ROOT/openspec/changes/keel-plugin-commands/evidence/ui-e2e}"
if [[ -e "$BASE_DIR" && ! -f "$BASE_DIR/report.html" ]]; then
  mv "$BASE_DIR" "$BASE_DIR.previous-$STAMP"
fi
RUN_DIR="$BASE_DIR"
CACHE="${KEEL_E2E_CACHE:-/tmp/keel-commands-e2e-build-$STAMP}"
SECRETS="$RUN_DIR/secrets"
RUNTIME="$RUN_DIR/runtime"
SHOTS="$RUN_DIR/shots"
DB="keel_commands_e2e_$(date +%s)"
TENANT="00000000-0000-0000-0000-00000000a003"
PG_USER="${KEEL_E2E_PG_USER:-$(whoami)}"
DSN_BASE="${KEEL_E2E_DSN_BASE:-postgres://$PG_USER@127.0.0.1:5432}"
KEEP_STACK="${KEEL_E2E_KEEP_STACK:-0}"
SKIP_BUILD="${KEEL_E2E_SKIP_BUILD:-0}"
SERVER_PID=""
PROVIDER_PID=""
DB_CREATED=0
PLUGIN_STATE="$RUNTIME/plugin-state"
# Record the build identity BEFORE this script rewrites its evidence: after
# that the worktree is legitimately dirty and the marker would be misleading.
HEAD_COMMIT="${E2E_HEAD_COMMIT:-$(cd "$ROOT" && git rev-parse HEAD)}"
if [[ -z "${E2E_HEAD_COMMIT:-}" && -n "$(cd "$ROOT" && git status --porcelain)" ]]; then
  HEAD_COMMIT="$HEAD_COMMIT-dirty"
fi

say() { printf '%s\n' "$*"; }
fail() { say "keel-commands-e2e: $*" >&2; exit 1; }

# A process is ours only when its environment carries THIS run's marker: a
# concurrent lane may run the same scripts, and killing by name alone would
# take its providers down too.
kill_matching() {
  local pattern="$1" marker="$2" pid
  for pid in $(pgrep -f "$pattern" 2>/dev/null || true); do
    if ps -eww -p "$pid" -o command= 2>/dev/null | grep -qF "$marker"; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
}

# Stop the server and every helper this run started. Idempotent: it runs once
# before the report (so the report can state the residue) and again on exit.
stop_aux() {
  local model_port="${MODEL_PORT:-0}"
  local plugin_state="${PLUGIN_STATE:-$RUN_DIR/runtime/plugin-state}"
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
  kill_matching "keel-commands-e2e/plugin/sidecar.mjs" "KEEL_COMMANDS_E2E_STATE=$plugin_state"
  kill_matching "keel-checkpoint-e2e/fake-provider.mjs" "E2E_MODEL_PORT=$model_port"
  sleep 0.3
}

# Residue proof: the run is only clean when its two ports are free and no
# provider/sidecar process survives. The report renders this file.
write_residue() {
  local port="${PORT:-0}"
  local model_port="${MODEL_PORT:-0}"
  local plugin_state="${PLUGIN_STATE:-$RUN_DIR/runtime/plugin-state}"
  {
    printf 'run=%s\n' "${DB:-unknown}"
    printf 'serve_port=%s\n' "$port"
    printf 'model_port=%s\n' "$model_port"
    if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
      printf 'serve_port=STILL_LISTENING\n'
    else
      printf 'serve_port=free\n'
    fi
    if lsof -nP -iTCP:"$model_port" -sTCP:LISTEN >/dev/null 2>&1; then
      printf 'model_port=STILL_LISTENING\n'
    else
      printf 'model_port=free\n'
    fi
    local sidecar_left=0 provider_left=0 pid
    for pid in $(pgrep -f "keel-commands-e2e/plugin/sidecar.mjs" 2>/dev/null || true); do
      if ps -eww -p "$pid" -o command= 2>/dev/null | grep -qF "KEEL_COMMANDS_E2E_STATE=$plugin_state"; then
        sidecar_left=1
      fi
    done
    for pid in $(pgrep -f "keel-checkpoint-e2e/fake-provider.mjs" 2>/dev/null || true); do
      if ps -eww -p "$pid" -o command= 2>/dev/null | grep -qF "E2E_MODEL_PORT=$model_port"; then
        provider_left=1
      fi
    done
    if [[ "$sidecar_left" == "1" ]]; then
      printf 'sidecar=STILL_RUNNING\n'
    else
      printf 'sidecar=gone\n'
    fi
    if [[ "$provider_left" == "1" ]]; then
      printf 'fake_provider=STILL_RUNNING\n'
    else
      printf 'fake_provider=gone\n'
    fi
  } > "$RUN_DIR/residue.txt" 2>/dev/null || true
}

cleanup() {
  local code=$?
  trap - EXIT INT TERM HUP QUIT
  stop_aux
  write_residue
  if [[ "$KEEP_STACK" == "1" ]]; then
    say "kept stack: db=$DB dir=$RUN_DIR"
  else
    if [[ "$DB_CREATED" == "1" ]]; then
      dropdb --force --if-exists "$DB" >/dev/null 2>&1 || true
    fi
    rm -rf "$SECRETS" "$RUN_DIR/dsn" 2>/dev/null || true
  fi
  python3 - "$RUN_DIR" "$DB" "${SERVER_PID:-}" "$KEEP_STACK" "$code" <<'PYCLEANUP' 2>/dev/null || true
import datetime, json, sys
run_dir, db, pid, keep, code = sys.argv[1:]
payload = {
    "database": db,
    "kept": keep == "1",
    "serverPid": pid,
    "residue": "residue.txt",
    "finishedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "exitCode": int(code),
}
with open(run_dir + "/cleanup.json", "w", encoding="utf-8") as fh:
    json.dump(payload, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
PYCLEANUP
  exit "$code"
}
trap cleanup EXIT INT TERM HUP QUIT

command -v go >/dev/null || fail "go is required"
command -v node >/dev/null || fail "node is required"
command -v psql >/dev/null || fail "psql (PostgreSQL client) is required"
command -v createdb >/dev/null || fail "createdb is required"
# A rerun owns its artifacts: stale screenshots, a previous plugin state log
# or an old serve.log would be mistaken for this run's evidence.
rm -rf "$RUN_DIR/shots" "$RUN_DIR/data" "$RUN_DIR/runtime" "$RUN_DIR/secrets"
rm -f "$RUN_DIR/steps.json" "$RUN_DIR/report.html" "$RUN_DIR/cleanup.json" "$RUN_DIR/dsn"
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
      "contextWindow": 60000,
      "inputFormat": {"text": true},
      "outputFormat": {"text": true},
      "supportsToolCall": true
    }}
  }]
}
JSON

cat > "$RUNTIME/config.toml" <<'TOML'
version = 1

# Manual compaction is the command under test; the automatic threshold is out
# of reach so /compact is the only thing that compacts during the run.
[compaction]
enabled = true
thresholdTokens = 1000000
keepRecentTokens = 100
maxCheckpoints = 3
ttlHours = 168
manualCompact = true
TOML

mkdir -p "$RUNTIME/home/skills" "$RUNTIME/cas" "$RUNTIME/plugins" "$RUNTIME/plugin-state"

# ---- deterministic provider ------------------------------------------------
say "starting fake provider on 127.0.0.1:$MODEL_PORT"
E2E_MODEL_PORT="$MODEL_PORT" node "$ROOT/scripts/keel-checkpoint-e2e/fake-provider.mjs" "$MODEL_PORT" \
  >"$RUNTIME/fake-provider.log" 2>&1 &
PROVIDER_PID=$!
# The run owns the provider; the shell's job-status line ("Killed: 9") is noise
# in the evidence log, and the PID is tracked for cleanup regardless.
disown "$PROVIDER_PID" 2>/dev/null || true
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
export E2E_HEAD_COMMIT="$HEAD_COMMIT"
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
export E2E_PLUGINS_ROOT="$RUNTIME/plugins"
export E2E_PLUGIN_SRC="$ROOT/scripts/keel-commands-e2e/plugin"
export E2E_PLUGIN_STATE="$PLUGIN_STATE"
export E2E_SECRET_FILE="$SECRETS/secret.key"
export E2E_CHROME="${E2E_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
export PLAYWRIGHT_CORE="${PLAYWRIGHT_CORE:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}"
export E2E_HEADLESS="${E2E_HEADLESS:-true}"
export E2E_REPO_ROOT="$ROOT"
export E2E_PUBLIC_URL="${E2E_PUBLIC_URL:-https://local-kzf-keel-live-gh.fde2e.com/commands/report.html}"

[[ -f "$E2E_CHROME" ]] || fail "Chrome not found: $E2E_CHROME"
[[ -f "$PLAYWRIGHT_CORE" ]] || fail "playwright-core not found: $PLAYWRIGHT_CORE"

set +e
node "$ROOT/scripts/keel-commands-e2e/e2e.mjs"
E2E_CODE=$?
set -e

# The browser lane stops the server itself; stop the provider/sidecars and
# write the residue proof BEFORE the report, so the HTML can state it.
stop_aux
write_residue
if [[ -f "$RUN_DIR/steps.json" ]]; then
  node "$ROOT/scripts/keel-commands-e2e/report.mjs" "$RUN_DIR" || fail "report generation failed"
else
  say "no steps.json produced" >&2
fi
say "evidence: $RUN_DIR"
say "steps exit: $E2E_CODE"
exit "$E2E_CODE"
