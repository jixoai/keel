#!/usr/bin/env bash
# Live GitHub E2E for change keel-collab-superagent (task 5.2).
#
# What this starts (nothing is shared with a developer deployment):
#   * a throwaway PostgreSQL database created with the local `createdb` and
#     dropped by this script;
#   * the real `keel serve --auth local-only` from this worktree, with
#     KEEL_SUPERAGENT_SOURCE=github against a throwaway GitHub repository whose
#     name starts with keel-test-;
#   * a loopback scripted OpenAI provider that drives the worker through the
#     real `ask` / `report` model tools;
#   * the packaged web build, so the /superagent board and ticket detail are
#     screenshot evidence from the product UI.
#
# The live Source/Channel never points outside the configured test repository;
# the token is read from `gh auth token` into the process environment only and
# is never written to a file, a log or a commit.
#
# Usage:
#   scripts/superagent-live-github/run.sh
#   KEEL_LIVE_REPO=owner/keel-test-... scripts/superagent-live-github/run.sh
#   KEEL_LIVE_SKIP_BUILD=1 KEEL_LIVE_SKIP_WEB=1 ...   # reuse bin/web
#   KEEL_LIVE_KEEP_STACK=1 ...                        # keep DB/server for review
#
# Requirements: go, node, gh (logged in with repo scope), psql/createdb/dropdb,
# Chrome, playwright-core, nsh (only when the web build is missing).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
RUN="${KEEL_LIVE_RUN_DIR:-$ROOT/.agents/reports/keel/superagent-live-github-$(date +%Y-%m-%d)}"
CACHE="${KEEL_LIVE_CACHE:-/tmp/keel-superagent-live-github-build}"
BIN="${KEEL_LIVE_BIN:-$CACHE/keel}"
WEB_ROOT="${KEEL_LIVE_WEB_DIR:-/tmp/keel-superagent-live-web/Keel}"
DB="keel_superagent_live_$(date +%s)"
TENANT="${KEEL_LIVE_TENANT:-00000000-0000-0000-0000-00000000b001}"
PG_USER="${KEEL_LIVE_PG_USER:-$(whoami)}"
DSN_BASE="${KEEL_LIVE_DSN_BASE:-postgres://$PG_USER@127.0.0.1:5432}"
SKIP_BUILD="${KEEL_LIVE_SKIP_BUILD:-0}"
SKIP_WEB="${KEEL_LIVE_SKIP_WEB:-0}"
KEEP_STACK="${KEEL_LIVE_KEEP_STACK:-0}"
PLAYWRIGHT_CORE="${KEEL_LIVE_PLAYWRIGHT:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}"
CHROME="${KEEL_LIVE_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"

mkdir -p "$RUN/runtime" "$RUN/secrets" "$RUN/shots" "$RUN/data"
# One lane, one evidence set: stale PNGs from an earlier attempt must not look
# like this run's screenshots.
rm -f "$RUN/shots"/*.png
chmod 700 "$RUN" "$RUN/secrets"
RUNTIME="$RUN/runtime"
SERVER_PID=""
PROVIDER_PID=""
DB_CREATED=0
ISSUE_CREATED=0

say() { printf '%s\n' "$*"; }
fail() { say "superagent-live-github: $*" >&2; exit 1; }

stop_pid() {
  local pid="$1"
  [[ -z "$pid" ]] && return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.1
  done
  kill -KILL "$pid" 2>/dev/null || true
}

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  stop_pid "${SERVER_PID:-}"
  stop_pid "${PROVIDER_PID:-}"
  if [[ "$KEEP_STACK" != "1" ]]; then
    if [[ "$DB_CREATED" == "1" ]]; then
      dropdb --force --if-exists "$DB" >/dev/null 2>&1 || true
    fi
  fi
  python3 - "$RUN" "$DB" "${SERVER_PID:-}" "${PROVIDER_PID:-}" "$KEEP_STACK" "$code" <<'PY' 2>/dev/null || true
import datetime, json, sys
run_dir, db, server_pid, provider_pid, keep, code = sys.argv[1:]
payload = {
    "database": db,
    "kept": keep == "1",
    "serverPid": server_pid,
    "providerPid": provider_pid,
    "finishedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "exitCode": int(code),
}
with open(run_dir + "/cleanup.json", "w", encoding="utf-8") as fh:
    json.dump(payload, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
PY
  say "evidence: $RUN"
  say "exit: $code"
  exit "$code"
}
trap cleanup EXIT INT TERM

command -v go >/dev/null || fail "go is required"
command -v node >/dev/null || fail "node is required"
command -v gh >/dev/null || fail "gh is required"
command -v psql >/dev/null || fail "psql is required"
command -v createdb >/dev/null || fail "createdb is required"

# api.github.com occasionally answers EOF from this network; the auth probe and
# the read-only identity call get the same short bounded retry.
AUTH_OK=0
for _ in $(seq 1 5); do
  if gh auth status >/dev/null 2>&1; then
    AUTH_OK=1
    break
  fi
  sleep 2
done
[[ "$AUTH_OK" == "1" ]] || fail "gh is not logged in"
LOGIN=""
for _ in $(seq 1 5); do
  if LOGIN="$(gh api user --jq .login 2>/dev/null)" && [[ -n "$LOGIN" ]]; then
    break
  fi
  sleep 2
done
[[ -n "$LOGIN" ]] || fail "cannot read the gh login"
gh auth status 2>&1 | grep -q "repo" || fail "the gh token has no repo scope"
say "gh account: $LOGIN"

# ---- test repository (only a keel-test-* repository is ever written) -------
REPO="${KEEL_LIVE_REPO:-}"
if [[ -z "$REPO" ]]; then
  REPO="keel-test-superagent-e2e-$STAMP"
  say "creating test repository $LOGIN/$REPO"
  gh repo create "$LOGIN/$REPO" --public --add-readme \
    --description "Keel super-agent live GitHub E2E (throwaway)" >/dev/null || fail "gh repo create failed"
fi
[[ "$REPO" == *"/keel-test-"* || "$REPO" == keel-test-* ]] || fail "refusing repo $REPO: the name must contain keel-test-"
say "repository: $REPO"

# ---- ports: the project's 13600-13799 window -------------------------------
pick_port() {
  local exclude="${1:-}"
  local candidate
  for candidate in $(seq 13760 13799) $(seq 13600 13759); do
    [[ "$candidate" == "$exclude" ]] && continue
    if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}
PORT="${KEEL_LIVE_PORT:-$(pick_port)}"
[[ -n "$PORT" ]] || fail "no free serve port in 13600-13799"
MODEL_PORT="${KEEL_LIVE_MODEL_PORT:-$(pick_port "$PORT")}"
[[ -n "$MODEL_PORT" ]] || fail "no free model port in 13600-13799"

# ---- build -----------------------------------------------------------------
if [[ "$SKIP_BUILD" != "1" ]]; then
  say "building keel -> $BIN"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/keel ) || fail "go build failed"
fi
[[ -x "$BIN" ]] || fail "missing keel binary: $BIN"
if [[ ! -f "$WEB_ROOT/index.html" ]]; then
  [[ "$SKIP_WEB" != "1" ]] || fail "missing web build: $WEB_ROOT (unset KEEL_LIVE_SKIP_WEB)"
  say "packaging web -> $WEB_ROOT"
  ( cd "$ROOT/web" && nsh package --target web -o "$(dirname "$WEB_ROOT")" ) || fail "nsh package failed"
fi
[[ -f "$WEB_ROOT/index.html" ]] || fail "missing web build: $WEB_ROOT"

# ---- database, secrets, model config --------------------------------------
say "creating database $DB"
dropdb --force --if-exists "$DB" >/dev/null 2>&1 || true
createdb "$DB" || fail "createdb failed"
DB_CREATED=1
DSN="$DSN_BASE/$DB?sslmode=disable"
printf '%s' "$DSN" > "$RUNTIME/dsn"
chmod 600 "$RUNTIME/dsn"
python3 - "$RUN/secrets" <<'PY'
import os, secrets, sys
path = os.path.join(sys.argv[1], "secret.key")
with open(path, "w", encoding="utf-8") as fh:
    fh.write(secrets.token_hex(32))
os.chmod(path, 0o600)
PY

cat > "$RUNTIME/models.json" <<JSON
{
  "providers": [{
    "id": "e2e",
    "name": "Live E2E Provider",
    "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$MODEL_PORT/v1"},
    "access": {"type": "api-key", "env": "LIVE_E2E_PROVIDER_KEY"},
    "modelIds": ["live-e2e-model"]
  }],
  "selection": {"providerId": "e2e", "modelId": "live-e2e-model"},
  "modelRules": [{
    "match": "live-e2e-model",
    "config": {"properties": {
      "contextWindow": 60000,
      "inputFormat": {"text": true},
      "outputFormat": {"text": true},
      "supportsToolCall": true
    }}
  }]
}
JSON
printf 'version = 1\n' > "$RUNTIME/config.toml"

# The deployment environment is an array, not a shell function: a function
# launched with `&` gives $! the subshell's pid and leaves the real server
# orphaned on TERM.
SERVE_ENV=(
  "KEEL_DSN=$DSN"
  "KEEL_SECRET_KEY=$(cat "$RUN/secrets/secret.key")"
  "KEEL_SERVE_TENANT=$TENANT"
  "KEEL_MODELS_CONFIG=$RUNTIME/models.json"
  "KEEL_CONFIG_FILE=$RUNTIME/config.toml"
  "KEEL_HOME=$RUNTIME/home"
  "KEEL_SKILLS_ROOT=$RUNTIME/home/skills"
  "KEEL_CAS_DIR=$RUNTIME/cas"
  "KEEL_SERVE_WEB_DIR=$WEB_ROOT"
  "LIVE_E2E_PROVIDER_KEY=live-e2e-key"
)

wait_health() {
  for _ in $(seq 1 150); do
    if curl -fsS --max-time 1 "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# ---- provider --------------------------------------------------------------
say "starting scripted provider on 127.0.0.1:$MODEL_PORT"
: > "$RUNTIME/provider.log"
node "$ROOT/scripts/superagent-live-github/provider.mjs" "$MODEL_PORT" "$RUNTIME/provider.log" \
  >"$RUNTIME/provider.out" 2>&1 &
PROVIDER_PID=$!
ready=0
for _ in $(seq 1 50); do
  if curl -fsS --max-time 1 "http://127.0.0.1:$MODEL_PORT/v1/models" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
[[ "$ready" == "1" ]] || fail "scripted provider did not become healthy"

# ---- bootstrap pass (the queue needs both Bots before it can assemble) -----
say "bootstrap serve: creating role/profile/bots/grants"
cd "$ROOT"
env "${SERVE_ENV[@]}" "$BIN" serve --addr "127.0.0.1:$PORT" --auth local-only --tenant "$TENANT" \
  >"$RUNTIME/serve-bootstrap.log" 2>&1 &
SERVER_PID=$!
wait_health || fail "bootstrap serve did not become healthy"
node "$ROOT/scripts/superagent-live-github/bootstrap.mjs" "http://127.0.0.1:$PORT" "$RUNTIME/bootstrap.json" \
  >"$RUNTIME/bootstrap.log" 2>&1 || fail "bootstrap failed (see $RUNTIME/bootstrap.log)"
stop_pid "$SERVER_PID"
SERVER_PID=""

# ---- real deployment with the GitHub Source enabled ------------------------
say "starting the super-agent deployment (source=github)"
: > "$RUNTIME/serve.log"
env "${SERVE_ENV[@]}" \
  KEEL_SUPERAGENT_SOURCE=github \
  KEEL_SUPERAGENT_GITHUB_REPO="$REPO" \
  KEEL_SUPERAGENT_GITHUB_TOKEN="$(gh auth token)" \
  KEEL_SUPERAGENT_ORCHESTRATOR_BOT=orchestrator \
  KEEL_SUPERAGENT_WORKER_BOT=worker \
  KEEL_SUPERAGENT_ALLOWED_ACTORS="$LOGIN" \
  KEEL_SUPERAGENT_INTERVAL=2s \
  KEEL_SUPERAGENT_SLOTS=1 \
  KEEL_SUPERAGENT_HUMAN_SLOTS=1 \
  "$BIN" serve --addr "127.0.0.1:$PORT" --auth local-only --tenant "$TENANT" \
  >>"$RUNTIME/serve.log" 2>&1 &
SERVER_PID=$!
wait_health || fail "enabled serve did not become healthy"
grep -q "super-agent queue enabled" "$RUNTIME/serve.log" || fail "the super-agent queue did not assemble (see $RUNTIME/serve.log)"

say "granting the worker Bot its report/ask Harness tools"
node "$ROOT/scripts/superagent-live-github/set-worker-tools.mjs" "http://127.0.0.1:$PORT" worker \
  >"$RUNTIME/worker-tools.log" 2>&1 || fail "worker tool grant failed"

# ---- real issue ------------------------------------------------------------
MARK="keel-live-e2e-$STAMP"
say "creating the live issue on $REPO"
ISSUE_URL=""
for _ in $(seq 1 5); do
  if ISSUE_URL="$(gh issue create --repo "$REPO" --title "Live E2E ticket $MARK" 2>"$RUNTIME/issue-create.err" \
    --body "Live super-agent E2E marker: $MARK

Please review the simulated deploy and ask before acting.")" && [[ -n "$ISSUE_URL" ]]; then
    break
  fi
  sleep 3
done
[[ -n "$ISSUE_URL" ]] || fail "gh issue create failed"
ISSUE="${ISSUE_URL##*/}"
ISSUE_CREATED=1
say "issue: $ISSUE_URL"
printf '%s\n' "$ISSUE_URL" > "$RUNTIME/issue-url"
printf '%s\n' "$MARK" > "$RUNTIME/marker"

# ---- driver ----------------------------------------------------------------
export LIVE_BASE="http://127.0.0.1:$PORT"
export LIVE_RUN_DIR="$RUN"
export LIVE_REPO="$REPO"
export LIVE_ISSUE="$ISSUE"
export LIVE_ISSUE_URL="$ISSUE_URL"
export LIVE_MARKER="$MARK"
export LIVE_PROVIDER_LOG="$RUNTIME/provider.log"
export LIVE_GITHUB_TOKEN="$(gh auth token)"
export LIVE_CHROME="$CHROME"
export LIVE_PLAYWRIGHT="$PLAYWRIGHT_CORE"
export LIVE_HEADLESS="${KEEL_LIVE_HEADLESS:-true}"
export LIVE_PORT="$PORT"
export LIVE_MODEL_PORT="$MODEL_PORT"

set +e
node "$ROOT/scripts/superagent-live-github/e2e.mjs"
CODE=$?
set -e

say "issue kept for review: $ISSUE_URL"
say "repository kept for review: https://github.com/$REPO"
say "issue created this run: $ISSUE_CREATED"
exit "$CODE"
