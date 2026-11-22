#!/usr/bin/env bash
# Isolated real-service E2E stack for change keel-bot-identity-access (A6).
#
# What this starts (nothing here is shared with a developer deployment):
#   * a throwaway PostgreSQL database on the existing local server
#     (keel_e2e_identity_<epoch>), created and dropped by this script;
#   * one fresh tenant UUID;
#   * one `keel serve --auth required` on a free port in 13600-13799;
#   * the production web build from `nsh package --target web`;
#   * a 0600 bootstrap token / secret key / account passwords directory that
#     never reaches stdout, steps.json or the report.
#
# The browser lane (playwright-core + the machine's Chrome) runs in
# `e2e.mjs`; this script only owns the process/DB lifecycle, then generates
# `steps.json` -> `report.html` with `report-gen.mjs`.
#
# Usage:
#   scripts/keel-identity-e2e/run.sh                # full build + run + report
#   KEEL_E2E_SKIP_BUILD=1 scripts/keel-identity-e2e/run.sh
#   KEEL_E2E_KEEP_STACK=1 ...                       # keep server/DB for debug
#
# Requirements: docker (the local keel-pg container), node, python3, nsh, go.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAIN="$(cd "$ROOT/../.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
RUN_DIR="${KEEL_E2E_RUN_DIR:-$MAIN/.agents/reports/keel/identity-access-e2e/$STAMP}"
SECRETS="$RUN_DIR/secrets"
LOG="$RUN_DIR/serve.log"
DB="keel_e2e_identity_$(date +%s)"
TENANT="$(python3 -c 'import uuid; print(uuid.uuid4())')"
PG_CONTAINER="${KEEL_E2E_PG_CONTAINER:-keel-pg}"
PG_USER="${KEEL_E2E_PG_USER:-postgres}"
ENV_FILE="${FDE2E_ENV_PERSONAL:-$MAIN/.env.personal}"
KEEP_STACK="${KEEL_E2E_KEEP_STACK:-0}"
SKIP_BUILD="${KEEL_E2E_SKIP_BUILD:-0}"
SERVER_PID=""
DB_CREATED=0
STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

say() { printf '%s\n' "$*"; }
fail() { say "keel-e2e: $*" >&2; exit 1; }

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  local port_free="not-checked" db_gone="not-checked" legacy_gone="not-checked" proc_gone="not-checked"
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ -n "$SERVER_PID" ]]; then
    if kill -0 "$SERVER_PID" 2>/dev/null; then proc_gone="no"; else proc_gone="yes"; fi
  fi
  if command -v lsof >/dev/null 2>&1 && [[ -n "${PORT:-}" ]]; then
    if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then port_free="no"; else port_free="yes"; fi
  fi
  if [[ "$KEEP_STACK" == "1" ]]; then
    say "kept stack: db=$DB port=${PORT:-} pid=${SERVER_PID:-} dir=$RUN_DIR"
  else
    if [[ "$DB_CREATED" == "1" ]]; then
      docker exec "$PG_CONTAINER" dropdb -U "$PG_USER" --force --if-exists "$DB" >/dev/null 2>&1 || true
    fi
    if docker exec "$PG_CONTAINER" psql -U "$PG_USER" -Atc \
        "select 1 from pg_database where datname='$DB'" 2>/dev/null | grep -q '^1$'; then
      db_gone="no"
    else
      db_gone="yes"
    fi
    if [[ -n "${LEGACY_DB:-}" ]]; then
      if docker exec "$PG_CONTAINER" psql -U "$PG_USER" -Atc \
          "select 1 from pg_database where datname='$LEGACY_DB'" 2>/dev/null | grep -q '^1$'; then
        legacy_gone="no"
      else
        legacy_gone="yes"
      fi
    fi
    rm -rf "$SECRETS" "$RUN_DIR/dsn" "$RUN_DIR/legacy-dsn" 2>/dev/null || true
  fi
  python3 - "$RUN_DIR" "$DB" "${PORT:-}" "${SERVER_PID:-}" "$KEEP_STACK" \
    "$port_free" "$db_gone" "$legacy_gone" "$proc_gone" "$STARTED_AT" "$code" <<'PY'
import json, sys, datetime
run_dir, db, port, pid, keep, port_free, db_gone, legacy_gone, proc_gone, started, code = sys.argv[1:]
payload = {
    "database": db,
    "port": port,
    "serverPid": pid,
    "kept": keep == "1",
    "portFree": port_free,
    "databaseDropped": db_gone,
    "legacyDatabaseDropped": legacy_gone,
    "serverExited": proc_gone,
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

[[ -f "$ENV_FILE" ]] || fail "missing env file: $ENV_FILE"
mkdir -p "$SECRETS" "$RUN_DIR"
chmod 700 "$SECRETS"
chmod 700 "$RUN_DIR"

# ---- 1. port: first free in the project's 13600-13799 window --------------
if [[ -n "${KEEL_E2E_PORT:-}" ]]; then
  PORT="$KEEL_E2E_PORT"
  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    fail "KEEL_E2E_PORT=$PORT is already listening"
  fi
else
  PORT=""
  for candidate in $(seq 13757 13799) $(seq 13600 13756); do
    if ! lsof -nP -iTCP:"$candidate" -sTCP:LISTEN >/dev/null 2>&1; then
      PORT="$candidate"
      break
    fi
  done
  [[ -n "$PORT" ]] || fail "no free port in 13600-13799"
fi

# ---- 2. DSN: derive the throwaway database from the personal DSN ----------
python3 - "$ENV_FILE" "$DB" "$RUN_DIR/dsn" <<'PY'
import re, sys, urllib.parse
env_file, db, out = sys.argv[1:]
dsn = ""
for line in open(env_file, encoding="utf-8"):
    m = re.match(r"^KEEL_DSN=(.*)$", line.strip())
    if m:
        dsn = m.group(1).strip().strip('"').strip("'")
        break
if not dsn:
    raise SystemExit("KEEL_DSN is not set in " + env_file)
parsed = urllib.parse.urlparse(dsn)
if parsed.scheme not in ("postgres", "postgresql"):
    raise SystemExit("KEEL_DSN is not a postgres URL")
open(out, "w", encoding="utf-8").write(urllib.parse.urlunparse(parsed._replace(path="/" + db)))
PY
chmod 600 "$RUN_DIR/dsn"
DSN="$(cat "$RUN_DIR/dsn")"

# ---- 3. secrets and build artifacts ---------------------------------------
python3 - "$SECRETS" <<'PY'
import secrets, sys, os
d = sys.argv[1]
def write(name, value):
    path = os.path.join(d, name)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(value)
    os.chmod(path, 0o600)
write("bootstrap.token", "ksetup_" + secrets.token_urlsafe(32))
write("secret.key", secrets.token_hex(32))
write("admin.password", secrets.token_urlsafe(18))
write("alice.password", secrets.token_urlsafe(18))
write("bob.password", secrets.token_urlsafe(18))
write("member.password", secrets.token_urlsafe(18))
PY

BIN="${KEEL_E2E_BIN:-$RUN_DIR/keel}"
WEB_DIR="${KEEL_E2E_WEB_DIR:-$RUN_DIR/web}"
if [[ "$SKIP_BUILD" != "1" ]]; then
  say "building keel -> $BIN"
  ( cd "$ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/keel ) || fail "go build failed"
  if [[ ! -f "$WEB_DIR/Keel/index.html" && ! -f "$WEB_DIR/index.html" ]]; then
    say "packaging web -> $WEB_DIR (this takes a few minutes)"
    ( cd "$ROOT/web" && nsh package --target web -o "$WEB_DIR" ) || fail "nsh package failed"
  fi
fi
[[ -x "$BIN" ]] || fail "missing keel binary: $BIN"
if [[ ! -f "$WEB_DIR/Keel/index.html" && ! -f "$WEB_DIR/index.html" ]]; then
  fail "missing web build: $WEB_DIR (set KEEL_E2E_SKIP_BUILD=0)"
fi

# ---- 4. database ----------------------------------------------------------
say "creating database $DB on $PG_CONTAINER"
docker exec "$PG_CONTAINER" dropdb -U "$PG_USER" --if-exists "$DB" >/dev/null 2>&1 || true
docker exec "$PG_CONTAINER" createdb -U "$PG_USER" "$DB" || fail "createdb failed"
DB_CREATED=1

# ---- 5. serve -------------------------------------------------------------
say "starting keel serve on 127.0.0.1:$PORT (evidence $RUN_DIR)"
KEEL_DSN="$DSN" \
KEEL_BOOTSTRAP_TOKEN="$(cat "$SECRETS/bootstrap.token")" \
KEEL_SECRET_KEY="$(cat "$SECRETS/secret.key")" \
KEEL_SERVE_TENANT="$TENANT" \
"$BIN" serve \
  --addr "127.0.0.1:$PORT" \
  --auth required \
  --web-dir "$WEB_DIR" >"$LOG" 2>&1 &
SERVER_PID=$!

READY=0
for _ in $(seq 1 100); do
  if curl -fsS --max-time 1 "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1; then
    READY=1
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    say "server exited before readiness; see $LOG" >&2
    tail -20 "$LOG" >&2 || true
    exit 1
  fi
  sleep 0.2
done
if [[ "$READY" != "1" ]]; then
  fail "server did not become healthy on 127.0.0.1:$PORT"
fi

# ---- 6. legacy-schema negative database (independent) ---------------------
LEGACY_DB="keel_e2e_identity_legacy_$(date +%s)"
docker exec "$PG_CONTAINER" dropdb -U "$PG_USER" --if-exists "$LEGACY_DB" >/dev/null 2>&1 || true
docker exec "$PG_CONTAINER" createdb -U "$PG_USER" "$LEGACY_DB" || fail "legacy createdb failed"
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$LEGACY_DB" -v ON_ERROR_STOP=1 -c \
  "CREATE TABLE public.agents (tenant_id uuid NOT NULL); INSERT INTO public.agents VALUES (gen_random_uuid());" \
  >/dev/null || fail "legacy fixture setup failed"
python3 - "$DSN" "$LEGACY_DB" "$RUN_DIR/legacy-dsn" <<'PY'
import sys, urllib.parse
dsn, db, out = sys.argv[1:]
p = urllib.parse.urlparse(dsn)
open(out, "w", encoding="utf-8").write(urllib.parse.urlunparse(p._replace(path="/" + db)))
PY
chmod 600 "$RUN_DIR/legacy-dsn"
LEGACY_DSN="$(cat "$RUN_DIR/legacy-dsn")"

# ---- 7. browser lane ------------------------------------------------------
export E2E_BASE="http://127.0.0.1:$PORT"
export E2E_REPO_ROOT="$ROOT"
export E2E_HEAD_COMMIT="$(cd "$ROOT" && git rev-parse HEAD)"
export E2E_EVIDENCE="$RUN_DIR"
export E2E_BOOTSTRAP_TOKEN_FILE="$SECRETS/bootstrap.token"
export E2E_ADMIN_USERNAME="e2e-admin"
export E2E_ADMIN_PASSWORD_FILE="$SECRETS/admin.password"
export E2E_ALICE_PASSWORD_FILE="$SECRETS/alice.password"
export E2E_BOB_PASSWORD_FILE="$SECRETS/bob.password"
export E2E_MEMBER_PASSWORD_FILE="$SECRETS/member.password"
export E2E_KEEL_BIN="$BIN"
export E2E_DSN_FILE="$RUN_DIR/dsn"
export E2E_DB="$DB"
export E2E_LEGACY_DSN_FILE="$RUN_DIR/legacy-dsn"
export E2E_LEGACY_DB="$LEGACY_DB"
export E2E_TENANT="$TENANT"
export E2E_PG_CONTAINER="$PG_CONTAINER"
export E2E_PG_USER="$PG_USER"
export E2E_CHROME="${E2E_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
export PLAYWRIGHT_CORE="${PLAYWRIGHT_CORE:-/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs}"
export E2E_HEADLESS="${E2E_HEADLESS:-true}"

set +e
node "$ROOT/scripts/keel-identity-e2e/e2e.mjs"
E2E_CODE=$?
set -e

# Drop the legacy fixture database as soon as the lane is done; the primary DB
# is dropped by cleanup.
docker exec "$PG_CONTAINER" dropdb -U "$PG_USER" --force --if-exists "$LEGACY_DB" >/dev/null 2>&1 || true

if [[ -f "$RUN_DIR/steps.json" ]]; then
  node "$ROOT/scripts/keel-identity-e2e/report-gen.mjs" "$RUN_DIR" || fail "report generation failed"
else
  say "no steps.json produced" >&2
fi

# Handoff: the report server and public tunnel are started by a separate
# command so this script can still be re-run; see publish.sh.
say "evidence: $RUN_DIR"
say "steps exit: $E2E_CODE"

if [[ "$KEEP_STACK" != "1" ]]; then
  # Stop the server now so cleanup's evidence is accurate instead of racing.
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$SERVER_PID" 2>/dev/null || true
  fi
fi

exit "$E2E_CODE"
