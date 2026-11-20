#!/usr/bin/env bash
# mcp-apps-crash.sh — reproducible G1/G2/G3 acceptance for feat-mcp-apps-events.
#
# Runs cmd/e2e -scenario mcp-apps twice against a real `keel serve` binary and a
# real stdio MCP server (the e2e binary's fake-mcp-server subcommand). The
# scenario SIGKILLs the server process group at three provable points, restarts
# it, and asserts the ledger, app catalogue, metrics and audit trail of the new
# process.
#
# Requirements:
#   KEEL_TEST_DSN   PostgreSQL DSN of a DEDICATED database. The name must carry
#                   e2e or test (the same guard cmd/e2e enforces for -reset);
#                   this script truncates it and refuses anything else.
#   port 13686      free (override with KEEL_MCP_APPS_PORT).
#
# Usage:
#   KEEL_TEST_DSN='postgres://…' scripts/mcp-apps-crash.sh
#   KEEL_EVIDENCE_DIR=/tmp/p6 scripts/mcp-apps-crash.sh
#
# The DSN is never printed; the evidence records only the database name.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD

EVID_DIR="${KEEL_EVIDENCE_DIR:-openspec/changes/feat-mcp-apps-events/evidence/p6/crash}"
PORT="${KEEL_MCP_APPS_PORT:-13686}"

say() { printf '%s\n' "$*"; }
fail() { say "FAIL: $*"; exit 1; }

# --- DSN + dedicated-database guard ----------------------------------------
DSN="${KEEL_TEST_DSN:-}"
if [ -z "$DSN" ] && [ -f .env.local ]; then
  DSN="$(grep -E '^KEEL_TEST_DSN=' .env.local | head -1 | cut -d= -f2- || true)"
fi
[ -n "$DSN" ] || fail "KEEL_TEST_DSN is not set (and .env.local has no KEEL_TEST_DSN)"
DB_NAME="$(printf '%s' "$DSN" | sed -E 's#^[A-Za-z0-9+.-]+://([^/]*@)?[^/]+/([^?]+).*#\2#')"
[ -n "$DB_NAME" ] && [ "$DB_NAME" != "$DSN" ] || fail "cannot parse the database name from KEEL_TEST_DSN"
case "$(printf '%s' "$DB_NAME" | tr 'A-Z' 'a-z')" in
  *e2e*|*test*) ;;
  *) fail "refusing to truncate database \"$DB_NAME\": its name must contain e2e or test" ;;
esac

# --- port guard -------------------------------------------------------------
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  fail "port $PORT is already in use: $(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN | tail -1)"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/keel-mcp-apps-crash.XXXXXX")"
cleanup() {
  local status=$?
  # The scenario kills its own process group; this is the last-resort sweep for
  # an interrupted run (exact binary paths under this run's temp dir only).
  pkill -f "$WORK/keel" >/dev/null 2>&1 || true
  pkill -f "$WORK/e2e fake-mcp-server" >/dev/null 2>&1 || true
  sleep 1
  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    printf 'port %s still held after cleanup\n' "$PORT" >>"$EVID_DIR/cleanup.log" 2>/dev/null || true
    status=1
  else
    printf 'port %s free\n' "$PORT" >>"$EVID_DIR/cleanup.log" 2>/dev/null || true
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$EVID_DIR"
: >"$EVID_DIR/cleanup.log"
: >"$EVID_DIR/SUMMARY.txt"
{
  echo "\$ go build -o <work>/keel ./cmd/keel"
  echo "\$ go build -o <work>/e2e ./cmd/e2e"
  echo "\$ <work>/e2e -scenario mcp-apps -keel-bin <work>/keel -dsn \$KEEL_TEST_DSN -reset -reset-confirm $DB_NAME -mcp-apps-port $PORT -out <runN>"
  echo "database: $DB_NAME"
  echo "port: $PORT"
  echo "started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >"$EVID_DIR/command.txt"

say "== building keel and e2e"
go build -o "$WORK/keel" ./cmd/keel
go build -o "$WORK/e2e" ./cmd/e2e

overall=0
for run in 1 2; do
  out="$EVID_DIR/run$run"
  rm -rf "$out"
  mkdir -p "$out"
  say "== run $run"
  set +e
  "$WORK/e2e" -scenario mcp-apps -keel-bin "$WORK/keel" -dsn "$DSN" \
    -reset -reset-confirm "$DB_NAME" -mcp-apps-port "$PORT" -out "$out" \
    >"$out/stdout.json" 2>"$out/stderr.txt"
  status=$?
  set -e
  if [ "$status" -eq 0 ] && ! grep -q '"pass": false' "$out/stdout.json"; then
    printf 'run %d: PASS\n' "$run" | tee -a "$EVID_DIR/SUMMARY.txt"
  else
    printf 'run %d: FAIL (exit %d)\n' "$run" "$status" | tee -a "$EVID_DIR/SUMMARY.txt"
    overall=1
  fi
  # The crash matrix must be exactly three REAL SIGKILLs of the process group.
  if ! kills=$(python3 - "$out/kill-points.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
assert len(rows) == 3, f"want 3 kill points, got {len(rows)}"
assert {row["point"] for row in rows} == {"G2a", "G2b", "G2c"}
for row in rows:
    assert row.get("signal") == "SIGKILL", row
    assert row.get("processGroup") is True, row
    assert row.get("killError") in (None, ""), row
print(len(rows))
PY
  ); then
    kills=0
    overall=1
  fi
  if [ "$kills" != "3" ]; then
    overall=1
  fi
  printf 'run %d: kills=%s (SIGKILL)\n' "$run" "$kills" | tee -a "$EVID_DIR/SUMMARY.txt"
done

# Consistency: both runs must report the same set of crash points and passing
# assertions. The app identity/hash are deterministic (fixture bytes and the
# derived appId), so a difference is a real behavioural difference.
if [ "$overall" -eq 0 ]; then
  for run in 1 2; do
    python3 - "$EVID_DIR/run$run/assertions.json" "$EVID_DIR/run$run.signature" <<'PY'
import json, re, sys

def normalize(check):
    # Process ids are run-specific; every other assertion is deterministic.
    return re.sub(r"pid \d+→\d+", "pid <pid>→<pid>", check)

rows = json.load(open(sys.argv[1]))
sig = [
    (row["name"], row["pass"], [normalize(c) for c in (row.get("checks") or ())])
    for row in rows
]
open(sys.argv[2], "w").write(json.dumps(sig, ensure_ascii=False, indent=1))
PY
  done
  if cmp -s "$EVID_DIR/run1.signature" "$EVID_DIR/run2.signature"; then
    say "run1 and run2 assertions are identical" | tee -a "$EVID_DIR/SUMMARY.txt"
  else
    say "run1 and run2 assertions differ" | tee -a "$EVID_DIR/SUMMARY.txt"
    overall=1
  fi
  rm -f "$EVID_DIR/run1.signature" "$EVID_DIR/run2.signature"
fi

if [ "$overall" -ne 0 ]; then
  say "MCP APPS ACCEPTANCE FAILED"
  exit 1
fi
say "MCP APPS ACCEPTANCE PASS (2/2 runs)"
