#!/usr/bin/env bash
# Start (or stop) the local Keel server used for UI review.
#
# Why this exists: the login passphrase, tenant id and database DSN are
# credentials. Passing them as `--password …` on a command line puts them in
# shell history and in every `ps` snapshot. They live in the personal env file
# (gitignored) instead; this script only *reads* the KEEL_* keys and hands them
# to the server through its environment.
#
#   scripts/serve-local.sh            # start in the foreground
#   scripts/serve-local.sh --build    # rebuild the binary first
#   scripts/serve-local.sh --stop     # stop a server started this way
#
# Environment:
#   FDE2E_ENV_PERSONAL   path to the env file (default: the main repo's
#                        .env.personal; this worktree resolves it three levels up)
#   KEEL_BIN             server binary (default /tmp/keel-app)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${KEEL_BIN:-/tmp/keel-app}"

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
  local pattern="keel-app serve"
  if pgrep -f "$pattern" >/dev/null 2>&1; then
    pkill -f "$pattern"
    echo "stopped: $pattern"
  else
    echo "not running: $pattern"
  fi
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

# Read only the KEEL_* assignments. The env file is shared with other tools and
# may contain prose or non-shell lines; executing it wholesale would run them.
while IFS= read -r line; do
  key="${line%%=*}"
  value="${line#*=}"
  # Strip one layer of matching quotes, as a shell assignment would.
  value="${value%\"}"; value="${value#\"}"
  value="${value%\'}"; value="${value#\'}"
  export "$key=$value"
done < <(grep -E '^KEEL_[A-Z0-9_]+=' "$PERSONAL")

: "${KEEL_SERVE_PASSWORD:?KEEL_SERVE_PASSWORD is not set in $PERSONAL}"
: "${KEEL_SERVE_TENANT:?KEEL_SERVE_TENANT is not set in $PERSONAL}"
: "${KEEL_DSN:?KEEL_DSN is not set in $PERSONAL}"

if [[ "${1:-}" == "--build" || ! -x "$BIN" ]]; then
  ( cd "$ROOT" && go build -o "$BIN" ./cmd/keel )
fi

ADDR="${KEEL_SERVE_ADDR:-127.0.0.1:13760}"
WEB_DIR="${KEEL_SERVE_WEB_DIR:-$ROOT/web/.nsh/out}"

echo "keel serve on $ADDR (tenant ${KEEL_SERVE_TENANT%%-*}…, web $WEB_DIR)"
exec "$BIN" serve \
  --addr "$ADDR" \
  --tenant "$KEEL_SERVE_TENANT" \
  --web-dir "$WEB_DIR"
