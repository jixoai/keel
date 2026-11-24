#!/usr/bin/env bash
# keel dev PostgreSQL — a user-local PostgreSQL 18 cluster, no Docker.
#
# Creates and runs a throwaway cluster inside the repository (`.dev/pg`,
# gitignored), bound to 127.0.0.1 on port 13741, with a random superuser
# password kept in `.dev/pg.pass` (0600). It never touches an existing
# PostgreSQL installation or data directory.
#
# Usage:
#   scripts/dev-pg.sh start     create (first run) and start; prints the DSN
#   scripts/dev-pg.sh stop      stop the cluster (data stays)
#   scripts/dev-pg.sh status    running state + DSN
#   scripts/dev-pg.sh dsn       print the DSN (for KEEL_DSN)
#   scripts/dev-pg.sh env       print `export KEEL_DSN=...` (eval it)
#   scripts/dev-pg.sh reset --yes  stop and delete the cluster (data lost)
#
# Overrides: KEEL_DEV_PG_DIR, KEEL_DEV_PG_PORT, KEEL_DEV_PG_DB,
#            KEEL_DEV_PG_USER.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
data="${KEEL_DEV_PG_DIR:-$root/.dev/pg}"
port="${KEEL_DEV_PG_PORT:-13741}"
db="${KEEL_DEV_PG_DB:-keel}"
user="${KEEL_DEV_PG_USER:-keel}"
passfile="$data.pass"
log="$data.log"

say() { printf '%s\n' "$*"; }
fail() { printf 'dev-pg: %s\n' "$*" >&2; exit 1; }

find_bins() {
  local bindir=""
  if command -v pg_config >/dev/null 2>&1; then
    bindir="$(pg_config --bindir 2>/dev/null || true)"
  fi
  if [ -n "$bindir" ] && [ -x "$bindir/initdb" ]; then
    PATH="$bindir:$PATH"
  fi
  for bin in initdb pg_ctl pg_isready createdb psql; do
    command -v "$bin" >/dev/null 2>&1 || fail "missing PostgreSQL tool '$bin' (install PostgreSQL 18, e.g. brew install postgresql@18)"
  done
  local major
  major="$(initdb --version | sed -E 's/.* ([0-9]+)(\.[0-9]+)*.*/\1/')"
  case "$major" in
    ''|*[!0-9]*) fail "could not parse initdb --version" ;;
  esac
  [ "$major" -ge 18 ] || fail "PostgreSQL 18+ is required (initdb reports $major)"
}

running() {
  [ -f "$data/PG_VERSION" ] && pg_ctl -D "$data" status >/dev/null 2>&1
}

port_busy() {
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
  else
    pg_isready -h 127.0.0.1 -p "$port" >/dev/null 2>&1
  fi
}

dsn() {
  [ -f "$passfile" ] || fail "no password file at $passfile (run start first)"
  local pass
  pass="$(cat "$passfile")"
  printf 'postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable\n' "$user" "$pass" "$port" "$db"
}

init_cluster() {
  mkdir -p "$(dirname "$data")"
  [ -f "$passfile" ] || { umask 077; openssl rand -hex 16 > "$passfile"; }
  chmod 600 "$passfile"
  say "initializing a PostgreSQL cluster in $data (superuser $user, loopback only)"
  initdb -D "$data" -U "$user" --pwfile="$passfile" \
    --auth-local=trust --auth-host=scram-sha-256 \
    --encoding=UTF8 --locale=C >/dev/null
}

start_cluster() {
  if [ ! -f "$data/PG_VERSION" ]; then
    init_cluster
  fi
  if ! running; then
    if port_busy; then
      fail "port $port is already in use by another process: $(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | sed -n 2p)"
    fi
    pg_ctl -D "$data" -l "$log" -o "-p $port -h 127.0.0.1 -k $data" -w start >/dev/null
  fi
  local tries=0
  until pg_isready -h 127.0.0.1 -p "$port" -U "$user" -d postgres >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ "$tries" -lt 50 ] || fail "cluster did not become ready; see $log"
    sleep 0.1
  done
  if ! psql -h "$data" -p "$port" -U "$user" -d postgres -tAc \
      "SELECT 1 FROM pg_database WHERE datname = '$db'" | grep -q 1; then
    createdb -h "$data" -p "$port" -U "$user" "$db"
  fi
  say "ready: $(dsn)"
}

case "${1:-start}" in
  start)
    find_bins
    start_cluster
    ;;
  stop)
    find_bins
    if running; then
      pg_ctl -D "$data" -m fast -w stop >/dev/null
      say "stopped (data kept in $data)"
    else
      say "not running"
    fi
    ;;
  status)
    find_bins
    if running; then
      say "running on 127.0.0.1:$port (data: $data)"
      say "dsn: $(dsn)"
    else
      say "not running (data: $data)"
    fi
    ;;
  dsn)
    dsn
    ;;
  env)
    printf "export KEEL_DSN='%s'\n" "$(dsn)"
    ;;
  reset)
    [ "${2:-}" = "--yes" ] || fail "reset deletes $data; pass --yes to confirm"
    find_bins
    if running; then
      pg_ctl -D "$data" -m fast -w stop >/dev/null
    fi
    if [ -f "$data/PG_VERSION" ]; then
      rm -rf "$data"
    fi
    rm -f "$passfile" "$log"
    say "cluster removed"
    ;;
  -h|--help|help)
    sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
    ;;
  *)
    fail "unknown command '$1' (start|stop|status|dsn|env|reset)"
    ;;
esac
