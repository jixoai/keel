#!/usr/bin/env bash
# acceptance-g5.sh — one command for the keel-ga §6 G5 battery.
#
#   perf   : 1000 SSE + 64 concurrent runs + 1MiB images inside a 2GB cgroup
#            (scripts/acceptance-g5-perf.sh)
#   chaos  : SIGKILL every 30s for 10 minutes + strict ledger audit
#            (scripts/acceptance-g5-chaos.sh)
#   report : self-contained HTML + public copy + HTTP 200 check
#            (scripts/g5-report.py)
#
# Usage:
#   scripts/acceptance-g5.sh [evidence-dir]
#
# The DSN is read from KEEL_TEST_DSN or .env.local and never written to the
# report or logs. The default evidence directory is the keel-ga change's G5
# evidence folder.
set -euo pipefail
cd "$(dirname "$0")/.."

EVID="${1:-${KEEL_G5_EVIDENCE_DIR:-openspec/changes/keel-ga/evidence/g5-2026-11-21}}"
mkdir -p "$EVID"
EVID=$(cd "$EVID" && pwd)

say() { printf '%s\n' "$*"; }

say "== G5 battery: $EVID =="

if [ "${KEEL_G5_SKIP_PERF:-0}" != "1" ]; then
  scripts/acceptance-g5-perf.sh "$EVID/perf"
fi
if [ "${KEEL_G5_SKIP_CHAOS:-0}" != "1" ]; then
  scripts/acceptance-g5-chaos.sh "$EVID/chaos"
fi

say "== write metadata"
python3 -I - "$EVID/meta.json" <<'PY'
import datetime
import json
import platform
import subprocess
import sys


def run(*cmd):
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=15).stdout
        return out.strip()
    except Exception:
        return ""


meta = {
    "generatedAt": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
    "commit": run("git", "rev-parse", "HEAD"),
    "dirty": bool(run("git", "status", "--porcelain")),
    "goVersion": run("go", "version"),
    "dockerVersion": run("docker", "version", "--format", "{{.Server.Version}}"),
    "pgVersion": run("docker", "exec", "keel-pg", "psql", "-U", "postgres", "-Atc", "select version()"),
    "host": platform.platform(),
    "arch": platform.machine(),
    "profile": {
        "sessions": 1000,
        "concurrentRuns": 64,
        "cgroup": "2g",
        "image": "1MiB every 60s",
        "fakeProviderDelayMS": 30000,
        "chaos": "SIGKILL every 30s for 600s; settle 20s; 16 sessions",
        "databases": ["keel_test_ga_g5", "keel_test_ga_g5_chaos"],
        "ports": [13750, 13751, 13760, 13761],
    },
    "reproduce": "KEEL_TEST_DSN=... scripts/acceptance-g5.sh",
}
with open(sys.argv[1], "w", encoding="utf-8") as fh:
    json.dump(meta, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
print(f"meta: {sys.argv[1]}")
PY

COMMIT=$(git rev-parse HEAD)
say "== render report"
python3 -I scripts/g5-report.py "$EVID" "$EVID/report.html" --commit "$COMMIT"

PUBLISH_DIR="${KEEL_G5_PUBLISH_DIR:-/Users/kzf/Development/GitHub/jixoai/fde2e/.agents/reports/manager/keel/keel-ga-g5}"
PUBLISH_FILE="$PUBLISH_DIR/report.html"
URL="${KEEL_G5_REPORT_URL:-https://local-gaubee-reports.fde2e.com/keel/keel-ga-g5/report.html}"
if [ "${KEEL_G5_SKIP_PUBLISH:-0}" != "1" ]; then
  say "== publish $PUBLISH_FILE"
  mkdir -p "$PUBLISH_DIR"
  cp "$EVID/report.html" "$PUBLISH_FILE"
  chmod 644 "$PUBLISH_FILE"
  HTTP_CODE=$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' "$URL" || true)
  {
    echo "url=$URL"
    echo "http_code=$HTTP_CODE"
    echo "checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "published_file=$PUBLISH_FILE"
  } >"$EVID/publication.txt"
  say "   HTTP $HTTP_CODE — $URL"
  [ "$HTTP_CODE" = "200" ] || { say "G5 PUBLISH FAIL: expected 200, got $HTTP_CODE"; exit 1; }
fi

say "G5 BATTERY PASS"
