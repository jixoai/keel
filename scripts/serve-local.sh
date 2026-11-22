#!/usr/bin/env bash
# Start (or stop) the local Keel server used for UI review.
#
# Authentication changed in keel-bot-identity-access (alpha zero compatibility):
# there is no deployment passphrase any more. A human signs in with a username
# and password; a machine presents a Bot key (`keel_bot_...`). The server signs
# its session cookie and CSRF token with KEEL_SECRET_KEY, so that key is a
# deployment secret and lives in the personal env file just like the DSN.
#
#   scripts/serve-local.sh            # start in the foreground
#   scripts/serve-local.sh --build    # rebuild the binary first
#   scripts/serve-local.sh --stop     # stop a server started this way
#
# Environment (read from the personal env file, never printed):
#   KEEL_DSN              PostgreSQL DSN (required)
#   KEEL_SERVE_TENANT     tenant UUID this process acts as (required)
#   KEEL_SECRET_KEY       32-byte base64/hex cookie/CSRF signing key
#                         (required unless KEEL_AUTH=local-only|off)
#   KEEL_SERVE_ADDR       listen address (default 127.0.0.1:13760)
#   KEEL_BOOTSTRAP_TOKEN  optional one-time setup token for a first boot
#                         (the container-friendly alternative to the CLI)
#   KEEL_AUTH             required|local-only|off (default required)
#   KEEL_SERVE_WEB_DIR    static web build directory
#   FDE2E_ENV_PERSONAL    path to the env file (default: the main repo's
#                         .env.personal; this worktree resolves it three levels up)
#   KEEL_BIN              server binary (default /tmp/keel-app)
#
# First start on an empty database: run `keel admin bootstrap` (or set
# KEEL_BOOTSTRAP_TOKEN before boot) and complete setup in the browser. The
# token is shown once and is never printed by this script.
#
# KEEL_SERVE_PASSWORD is retired and refused by the product. This script does
# not read it from the env file and never forwards it to the server.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${KEEL_BIN:-/tmp/keel-app}"
PIDFILE="${KEEL_SERVE_PIDFILE:-/tmp/keel-serve-local.pid}"

# The worktree's own .env.local wins (easy to open, gitignored); the main
# repo's .env.personal is the shared fallback.
worktree_env() {
  local candidate="$ROOT/.env.local"
  [[ -f "$candidate" ]] && printf '%s\n' "$candidate"
}

main_repo_env() {
  # <main>/.worktrees/<name>/scripts/../.. = <main>
  local candidate="$ROOT/../../.env.personal"
  if [[ -f "$candidate" ]]; then
    printf '%s\n' "$candidate"
    return 0
  fi
  return 1
}

PERSONAL="${FDE2E_ENV_PERSONAL:-$(worktree_env || main_repo_env || true)}"

stop_server() {
  local pid=""
  if [[ -f "$PIDFILE" ]]; then
    pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  fi
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    local command
    command="$(ps -p "$pid" -o command= 2>/dev/null || true)"
    if [[ "$command" != *"serve"* ]]; then
      rm -f "$PIDFILE"
      echo "stale pid file: pid $pid is not a keel serve process" >&2
      return 1
    fi
    kill "$pid"
    for _ in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
      echo "still running: pid $pid (not forcing; inspect it first)" >&2
      return 1
    fi
    rm -f "$PIDFILE"
    echo "stopped: pid $pid"
    return 0
  fi
  rm -f "$PIDFILE"
  echo "not running: no live pid in $PIDFILE"
}

case "${1:-}" in
  --stop)
    stop_server
    exit 0
    ;;
esac

if [[ ! -f "$PERSONAL" ]]; then
  echo "missing env file: $PERSONAL" >&2
  echo "set FDE2E_ENV_PERSONAL or create the main repo's .env.personal" >&2
  exit 1
fi

# Read only KEY-shaped assignments (KEEL_* plus provider credential keys like
# S2API_KEY / DEEPSEEK_API_KEY — the shape the builtin catalog declares in
# `access.env`). The env file is shared with other tools and may contain prose
# or non-shell lines; executing it wholesale would run them. The retired
# KEEL_SERVE_PASSWORD is excluded and never exported.
while IFS= read -r line; do
  key="${line%%=*}"
  value="${line#*=}"
  # Strip one layer of matching quotes, as a shell assignment would.
  value="${value%\"}"; value="${value#\"}"
  value="${value%\'}"; value="${value#\'}"
  export "$key=$value"
done < <(grep -E '^(KEEL_[A-Z0-9_]+|[A-Z][A-Z0-9_]*_KEY)=' "$PERSONAL" | grep -v '^KEEL_SERVE_PASSWORD=')

: "${KEEL_DSN:?KEEL_DSN is not set in $PERSONAL}"
: "${KEEL_SERVE_TENANT:?KEEL_SERVE_TENANT is not set in $PERSONAL}"

AUTH_MODE="${KEEL_AUTH:-required}"
case "$AUTH_MODE" in
  required)
    : "${KEEL_SECRET_KEY:?KEEL_SECRET_KEY is required for auth=required (32 bytes, base64 or hex)}"
    ;;
  local-only|off)
    if [[ "$AUTH_MODE" == "off" && "${KEEL_INSECURE:-}" != "1" ]]; then
      echo "KEEL_AUTH=off requires KEEL_INSECURE=1 (loopback only; never expose it)" >&2
      exit 1
    fi
    ;;
  *)
    echo "KEEL_AUTH must be required|local-only|off (got $AUTH_MODE)" >&2
    exit 2
    ;;
esac

if [[ "${1:-}" == "--build" || ! -x "$BIN" ]]; then
  ( cd "$ROOT" && go build -o "$BIN" ./cmd/keel )
fi

ADDR="${KEEL_SERVE_ADDR:-127.0.0.1:13760}"
WEB_DIR="${KEEL_SERVE_WEB_DIR:-$ROOT/web/.nsh/out}"

echo "keel serve on $ADDR (tenant ${KEEL_SERVE_TENANT%%-*}…, auth $AUTH_MODE, web $WEB_DIR)"
if [[ "$AUTH_MODE" == "required" ]]; then
  if [[ -n "${KEEL_BOOTSTRAP_TOKEN:-}" ]]; then
    echo "bootstrap token: loaded from KEEL_BOOTSTRAP_TOKEN (one-time; value not printed)"
  else
    echo "first start on an empty database: run \`KEEL_DSN=… KEEL_SERVE_TENANT=$KEEL_SERVE_TENANT $BIN admin bootstrap\`,"
    echo "then complete setup in the browser with the one-time token it prints."
  fi
  echo "sign in with an account username + password; machine clients use a Bot key (KEEL_AGENT_TOKEN)."
fi

echo "$$" > "$PIDFILE"
exec "$BIN" serve \
  --addr "$ADDR" \
  --tenant "$KEEL_SERVE_TENANT" \
  --auth "$AUTH_MODE" \
  --web-dir "$WEB_DIR"
