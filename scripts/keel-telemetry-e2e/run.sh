#!/usr/bin/env bash
# Live E2E for change keel-plugin-telemetry (task 7.4): a real `keel serve`
# process, a throwaway PostgreSQL cluster, a deterministic OpenAI-compatible
# model and an independent OTLP/HTTP JSON collector.
#
# Nothing is shared with a developer deployment:
#   * PostgreSQL is a disposable cluster created by `keel db start` under a
#     temporary directory (never the system cluster);
#   * `keel serve --auth local-only` binds one free port in 13600-13799 and the
#     managed plugins root is temporary;
#   * the OTLP collector and the scripted model are child processes of this
#     script; every child is terminated and waited for on exit.
#
# Evidence (script-generated, never hand-written):
#   evidence/live-e2e/steps.json              step/assertion journal
#   evidence/live-e2e/report.html             rendered by report.mjs
#   evidence/live-e2e/collector-requests.jsonl full OTLP requests (secret redacted)
#   evidence/live-e2e/serve.log               real serve output
#   evidence/live-e2e/residue.txt             process/port/DB/secret-scan facts
#   evidence/live-e2e/run.log                 this script's output
#
# Usage:
#   scripts/keel-telemetry-e2e/run.sh
#   KEEL_E2E_SKIP_BUILD=1 KEEL_E2E_BIN=/tmp/keel scripts/keel-telemetry-e2e/run.sh
#   KEEL_E2E_KEEP_STACK=1 ...                 # keep the stack for debugging
#
# Requirements: go (unless a binary is supplied), node, python3, curl, lsof,
# pgrep, psql client and the PostgreSQL server tools (initdb/pg_ctl).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EVIDENCE="${KEEL_E2E_EVIDENCE:-$ROOT/openspec/changes/keel-plugin-telemetry/evidence/live-e2e}"
STAMP="$(date +%Y%m%d-%H%M%S)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/keel-telemetry-e2e.$STAMP.XXXXXX")"
SECRETS="$WORK/secrets"
KEEP_STACK="${KEEL_E2E_KEEP_STACK:-0}"
SKIP_BUILD="${KEEL_E2E_SKIP_BUILD:-0}"
DB_NAME="keel_telemetry_e2e_$(date +%s)"
TENANT="$(python3 -c 'import uuid; print(uuid.uuid4())')"
BIN="${KEEL_E2E_BIN:-$WORK/keel}"
SERVER_PID=""
MODEL_PID=""
COLLECTOR_PID=""
PG_STARTED=0
TEARDOWN_DONE=0
TEARDOWN_FAILURE=0
E2E_CODE=1

say() { printf '%s\n' "$*"; }
fail() { say "keel-telemetry-e2e: $*" >&2; exit 1; }

rm -rf "$EVIDENCE"
mkdir -p "$EVIDENCE" "$SECRETS" "$WORK/home" "$WORK/plugins"
chmod 700 "$WORK" "$SECRETS"
# Capture this script's output as evidence without hiding it from the caller.
exec > >(tee -a "$EVIDENCE/run.log") 2>&1

# ---- helper functions ------------------------------------------------------

pick_port() {
  local exclude=" $* "
  local candidate
  for candidate in $(seq 13600 13799); do
    case "$exclude" in
      *" $candidate "*) continue ;;
    esac
    if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

port_state() {
  local port="$1"
  if [[ -z "$port" ]]; then
    printf 'unknown\n'
  elif lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    printf 'listening\n'
  else
    printf 'free\n'
  fi
}

wait_http() {
  local url="$1" pid="$2" name="$3" log="$4"
  local i
  for i in $(seq 1 240); do
    if curl -fsS --max-time 1 "$url" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      say "$name exited before readiness" >&2
      tail -40 "$log" >&2 2>/dev/null || true
      return 1
    fi
    sleep 0.25
  done
  say "$name did not become healthy: $url" >&2
  tail -40 "$log" >&2 2>/dev/null || true
  return 1
}

# Stops one child we started: TERM, bounded wait, KILL, wait (reap), then a
# liveness fact. It never touches a process it did not receive a pid for.
stop_child() {
  local pid="$1" name="$2" residue="$3"
  if [[ -z "$pid" ]]; then
    printf '%s.pid=none %s.alive=n/a\n' "$name" "$name" >> "$residue"
    return 0
  fi
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  fi
  wait "$pid" 2>/dev/null || true
  if kill -0 "$pid" 2>/dev/null; then
    printf '%s.pid=%s %s.alive=yes\n' "$name" "$pid" "$name" >> "$residue"
    TEARDOWN_FAILURE=1
  else
    printf '%s.pid=%s %s.alive=no\n' "$name" "$pid" "$name" >> "$residue"
  fi
}

pgrep_fact() {
  local label="$1" pattern="$2" residue="$3"
  local pids
  pids="$(pgrep -f "$pattern" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
  if [[ -n "$pids" ]]; then
    printf 'pgrep.%s=%s\n' "$label" "$pids" >> "$residue"
    TEARDOWN_FAILURE=1
  else
    printf 'pgrep.%s=none\n' "$label" >> "$residue"
  fi
}

scan_secret() {
  local label="$1" file="$2" residue="$3"
  if [[ ! -f "$file" ]]; then
    printf 'secretScan.%s=file-missing\n' "$label" >> "$residue"
    TEARDOWN_FAILURE=1
  elif grep -R -F -q -f "$file" "$EVIDENCE" 2>/dev/null; then
    printf 'secretScan.%s=leaked=yes\n' "$label" >> "$residue"
    TEARDOWN_FAILURE=1
  else
    printf 'secretScan.%s=leaked=no\n' "$label" >> "$residue"
  fi
}

teardown() {
  [[ "$TEARDOWN_DONE" == "1" ]] && return 0
  TEARDOWN_DONE=1
  local residue="$EVIDENCE/residue.txt"
  : > "$residue"
  {
    printf 'keel telemetry live E2E residue\n'
    printf 'generatedAt=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'workDir=%s\n' "$WORK"
    printf 'database=%s\n' "$DB_NAME"
    printf 'ports: serve=%s model=%s collector=%s postgres=%s\n' "${PORT:-}" "${MODEL_PORT:-}" "${COLLECTOR_PORT:-}" "${PG_PORT:-}"
  } >> "$residue"

  if [[ "$KEEP_STACK" == "1" ]]; then
    printf 'kept=yes (KEEL_E2E_KEEP_STACK=1; manual cleanup required)\n' >> "$residue"
    say "kept stack: work=$WORK ports serve=${PORT:-} model=${MODEL_PORT:-} collector=${COLLECTOR_PORT:-} postgres=${PG_PORT:-}"
    return 0
  fi

  say "stopping serve/model/collector"
  stop_child "$SERVER_PID" serve "$residue"
  stop_child "$MODEL_PID" model "$residue"
  stop_child "$COLLECTOR_PID" collector "$residue"

  if [[ "$PG_STARTED" == "1" ]]; then
    KEEL_HOME="$WORK/home" "$BIN" db stop --dir "$WORK/pg" >/dev/null 2>&1 || true
    local pg_alive="no"
    if [[ -f "$WORK/pg/postmaster.pid" ]]; then
      local pg_pid
      pg_pid="$(head -1 "$WORK/pg/postmaster.pid" 2>/dev/null || true)"
      if [[ -n "$pg_pid" ]] && kill -0 "$pg_pid" 2>/dev/null; then
        pg_alive="yes"
        TEARDOWN_FAILURE=1
      fi
    fi
    printf 'postgres.alive=%s\n' "$pg_alive" >> "$residue"
    pgrep_fact postgres "$WORK/pg" "$residue"
    rm -rf "$WORK/pg"
    printf 'postgres.clusterRemoved=yes\n' >> "$residue"
  else
    printf 'postgres.started=no\n' >> "$residue"
  fi

  printf 'servePort.state=%s\n' "$(port_state "${PORT:-}")" >> "$residue"
  printf 'modelPort.state=%s\n' "$(port_state "${MODEL_PORT:-}")" >> "$residue"
  printf 'collectorPort.state=%s\n' "$(port_state "${COLLECTOR_PORT:-}")" >> "$residue"
  printf 'postgresPort.state=%s\n' "$(port_state "${PG_PORT:-}")" >> "$residue"
  for state_label in servePort.state modelPort.state collectorPort.state postgresPort.state; do
    if grep -q "^$state_label=listening$" "$residue"; then
      TEARDOWN_FAILURE=1
    fi
  done

  pgrep_fact serve "$BIN serve --addr 127.0.0.1:${PORT:-0}" "$residue"
  pgrep_fact model "keel-telemetry-e2e/fake-provider.mjs ${MODEL_PORT:-0}" "$residue"
  pgrep_fact collector "keel-telemetry-e2e/fake-collector.mjs ${COLLECTOR_PORT:-0}" "$residue"

  # Evidence must never carry the collector Authorization value or the admin key.
  scan_secret otlpToken "$SECRETS/otlp.token" "$residue"
  scan_secret adminKey "$SECRETS/admin.key" "$residue"

  if [[ -d "$WORK" ]]; then
    rm -rf "$WORK"
  fi
  if [[ -d "$WORK" ]]; then
    printf 'workDir.removed=no\n' >> "$residue"
    TEARDOWN_FAILURE=1
  else
    printf 'workDir.removed=yes\n' >> "$residue"
  fi
  if [[ "$TEARDOWN_FAILURE" == "0" ]]; then
    printf 'teardown=clean\n' >> "$residue"
  else
    printf 'teardown=residue-detected\n' >> "$residue"
  fi

  if [[ -f "$EVIDENCE/steps.json" ]]; then
    say "rendering report.html"
    if ! node "$ROOT/scripts/keel-telemetry-e2e/report.mjs" "$EVIDENCE"; then
      say "report generation failed" >&2
      printf 'report=failed\n' >> "$residue"
      TEARDOWN_FAILURE=1
    fi
  else
    printf 'report=no-steps-json\n' >> "$residue"
  fi
}

on_exit() {
  local code=$?
  trap - EXIT INT TERM
  teardown
  if [[ "$code" == "0" && "$TEARDOWN_FAILURE" != "0" ]]; then
    code=1
  fi
  say "evidence: $EVIDENCE"
  say "exit: $code"
  exit "$code"
}
trap on_exit EXIT INT TERM

# ---- 1. ports and build ----------------------------------------------------

PORT="$(pick_port)" || fail "no free serve port in 13600-13799"
MODEL_PORT="$(pick_port "$PORT")" || fail "no free model port in 13600-13799"
COLLECTOR_PORT="$(pick_port "$PORT" "$MODEL_PORT")" || fail "no free collector port in 13600-13799"
PG_PORT="$(pick_port "$PORT" "$MODEL_PORT" "$COLLECTOR_PORT")" || fail "no free PostgreSQL port in 13600-13799"
say "ports: serve=$PORT model=$MODEL_PORT collector=$COLLECTOR_PORT postgres=$PG_PORT"
say "tenant: $TENANT"

command -v node >/dev/null || fail "node is required"
command -v python3 >/dev/null || fail "python3 is required"
command -v curl >/dev/null || fail "curl is required"
command -v lsof >/dev/null || fail "lsof is required"
command -v pgrep >/dev/null || fail "pgrep is required"
command -v psql >/dev/null || fail "psql (PostgreSQL client) is required"
command -v initdb >/dev/null || fail "initdb (PostgreSQL server tools) is required"
command -v pg_ctl >/dev/null || fail "pg_ctl (PostgreSQL server tools) is required"

BIN="${KEEL_E2E_BIN:-$WORK/keel}"
if [[ "$SKIP_BUILD" != "1" ]]; then
  say "building keel -> $BIN"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/keel ) || fail "go build failed"
fi
[[ -x "$BIN" ]] || fail "missing keel binary: $BIN"

# ---- 2. throwaway PostgreSQL ----------------------------------------------

say "starting throwaway PostgreSQL on 127.0.0.1:$PG_PORT (database $DB_NAME)"
KEEL_HOME="$WORK/home" "$BIN" db start \
  --dir "$WORK/pg" --port "$PG_PORT" --database "$DB_NAME" --user keel \
  >"$EVIDENCE/db-start.log" 2>&1 || { tail -40 "$EVIDENCE/db-start.log" >&2; fail "keel db start failed"; }
PG_STARTED=1
DSN="$(KEEL_HOME="$WORK/home" "$BIN" db dsn --dir "$WORK/pg" 2>/dev/null)" || fail "keel db dsn failed"
[[ -n "$DSN" ]] || fail "empty DSN"
printf '%s\n' "$DSN" > "$WORK/dsn"
chmod 600 "$WORK/dsn"
say "PostgreSQL ready: $(psql "$DSN" -Atc 'select version()' | head -1)"

# ---- 3. secrets and model config ------------------------------------------

python3 - "$SECRETS" <<'PY'
import os
import secrets
import sys

directory = sys.argv[1]
os.makedirs(directory, mode=0o700, exist_ok=True)

def write(name, value):
    path = os.path.join(directory, name)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(value)
    os.chmod(path, 0o600)

write("otlp.token", "Bearer e2e-otlp-" + secrets.token_hex(16))
write("admin.key", secrets.token_hex(32))
write("secret.key", secrets.token_hex(32))
PY

python3 - "$MODEL_PORT" "$WORK/models.json" <<'PY'
import json
import sys

port, out = sys.argv[1], sys.argv[2]
document = {
    "providers": [{
        "id": "e2e",
        "name": "E2E Provider",
        "api": {"type": "openai-chat-completions", "baseUrl": f"http://127.0.0.1:{port}/v1"},
        "access": {"type": "api-key", "env": "E2E_PROVIDER_KEY"},
        "modelIds": ["e2e-model"],
    }],
    "selection": {"providerId": "e2e", "modelId": "e2e-model"},
    "modelRules": [{
        "match": "e2e-model",
        "config": {"properties": {
            "contextWindow": 30000,
            "inputFormat": {"text": True},
            "outputFormat": {"text": True},
            "supportsToolCall": True,
        }},
    }],
}
with open(out, "w", encoding="utf-8") as handle:
    json.dump(document, handle, indent=2)
    handle.write("\n")
PY

# ---- 4. independent OTLP collector and scripted model ---------------------

say "starting fake OTLP collector on 127.0.0.1:$COLLECTOR_PORT"
COLLECTOR_LOG="$EVIDENCE/collector-requests.jsonl" \
  node "$ROOT/scripts/keel-telemetry-e2e/fake-collector.mjs" "$COLLECTOR_PORT" \
  >"$EVIDENCE/collector.log" 2>&1 &
COLLECTOR_PID=$!
wait_http "http://127.0.0.1:$COLLECTOR_PORT/health" "$COLLECTOR_PID" "collector" "$EVIDENCE/collector.log" || fail "collector did not become healthy"

say "starting scripted model on 127.0.0.1:$MODEL_PORT"
MODEL_LOG="$EVIDENCE/model-requests.jsonl" \
  node "$ROOT/scripts/keel-telemetry-e2e/fake-provider.mjs" "$MODEL_PORT" \
  >"$EVIDENCE/fake-provider.log" 2>&1 &
MODEL_PID=$!
wait_http "http://127.0.0.1:$MODEL_PORT/health" "$MODEL_PID" "model" "$EVIDENCE/fake-provider.log" || fail "model did not become healthy"

# ---- 5. real keel serve ----------------------------------------------------

say "starting keel serve on 127.0.0.1:$PORT"
KEEL_HOME="$WORK/home" \
KEEL_DSN="$DSN" \
KEEL_SECRET_KEY="$(cat "$SECRETS/secret.key")" \
KEEL_SERVE_TENANT="$TENANT" \
KEEL_MODELS_CONFIG="$WORK/models.json" \
KEEL_ADMIN_KEY="$(cat "$SECRETS/admin.key")" \
KEEL_PLUGINS_ROOT="$WORK/plugins" \
E2E_PROVIDER_KEY="e2e-model-key" \
  "$BIN" serve --addr "127.0.0.1:$PORT" --auth local-only --tenant "$TENANT" --plugins-root "$WORK/plugins" \
  >"$EVIDENCE/serve.log" 2>&1 &
SERVER_PID=$!
wait_http "http://127.0.0.1:$PORT/v1/health" "$SERVER_PID" "keel serve" "$EVIDENCE/serve.log" || fail "keel serve did not become healthy"
say "keel serve healthy (pid $SERVER_PID)"

# ---- 6. driver -------------------------------------------------------------

export E2E_BASE="http://127.0.0.1:$PORT"
export E2E_EVIDENCE="$EVIDENCE"
export E2E_TENANT="$TENANT"
export E2E_COLLECTOR_LOG="$EVIDENCE/collector-requests.jsonl"
export E2E_MODEL_LOG="$EVIDENCE/model-requests.jsonl"
export E2E_OTLP_TOKEN_FILE="$SECRETS/otlp.token"
export E2E_ADMIN_KEY_FILE="$SECRETS/admin.key"
export E2E_DSN_FILE="$WORK/dsn"
export E2E_COLLECTOR_PORT="$COLLECTOR_PORT"
export E2E_MODEL_PORT="$MODEL_PORT"
export E2E_HEAD_COMMIT="$(cd "$ROOT" && git rev-parse HEAD)"

say "running live E2E driver"
node "$ROOT/scripts/keel-telemetry-e2e/e2e.mjs"
E2E_CODE=$?

teardown
if [[ "$E2E_CODE" == "0" && "$TEARDOWN_FAILURE" != "0" ]]; then
  E2E_CODE=1
fi
exit "$E2E_CODE"
