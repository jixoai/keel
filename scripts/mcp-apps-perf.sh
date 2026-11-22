#!/usr/bin/env bash
# MCP Apps G5 acceptance: scale, 2 GB cgroup and chaos (change
# feat-mcp-apps-events, design §5.2).
#
# What it proves, against a real PostgreSQL database and a real `keel serve`
# process under --memory=2g:
#
#   Scale   1000 sessions (100 vs 1000 snapshots) at the configured 64 run
#           slots; the app catalogue is byte-identical at both points, the
#           mcp_* series set does not change, and the in-use heap attributed
#           to internal/mcphost does not grow with the session count.
#   Cache   20 app documents of ~1.5 MiB churned through 10 generations
#           (300 MiB of content): the 64 MiB byte-bounded LRU holds, observed
#           from a heap profile, while the catalogue stays complete.
#   Bridge  200 frames/s for 60 s across 20 app ids through the real HTTP
#           proxy surface; 0 x 429/5xx. A deliberate burst produces a 429 and
#           an audit row; five concurrent slow calls exercise the per-app
#           concurrency bound of 4.
#   Chaos   10 minutes: random SIGKILLs of the MCP server process and random
#           listen-stream drops. The runtime stays alive, the catalogue
#           recomputes after each restart, the listen supervisor reconnects
#           within the 30 s backoff cap (a reject-listen window measures the
#           schedule), reconnects/audit/diagnostics are counted, and the
#           cgroup high-water mark stays below 1.8 GiB.
#
# Isolation: a dedicated database, loopback-only ports inside a private docker
# network namespace, and a dedicated 2 GB cgroup. Everything is removed on
# exit; the evidence directory keeps the JSON/profiles/logs.
#
# Usage: scripts/mcp-apps-perf.sh
# Overrides (smoke runs): KEEL_P6D_SESSIONS, KEEL_P6D_FIRST_BATCH,
# KEEL_P6D_SSE_CAP, KEEL_P6D_BRIDGE_DURATION, KEEL_P6D_CHAOS_DURATION,
# KEEL_P6D_CACHE_GENERATIONS, KEEL_P6D_EVIDENCE, KEEL_P6D_RUN_DIR.
#
# Fail-closed: a missing cgroup peak, any threshold miss, a non-zero load
# result or an unreadable profile writes verdict FAIL and exits non-zero.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${KEEL_P6D_EVIDENCE:-$ROOT/openspec/changes/feat-mcp-apps-events/evidence/p6/perf}"
RUN_DIR="${KEEL_P6D_RUN_DIR:-/tmp/keel-p6d-perf}"
SESSIONS="${KEEL_P6D_SESSIONS:-1000}"
FIRST_BATCH="${KEEL_P6D_FIRST_BATCH:-100}"
SSE_CAP="${KEEL_P6D_SSE_CAP:-256}"
RUN_SLOTS="${KEEL_P6D_RUN_SLOTS:-64}"
APPS="${KEEL_P6D_APPS:-20}"
DOC_BYTES="${KEEL_P6D_DOC_BYTES:-1572864}"
BRIDGE_RATE="${KEEL_P6D_BRIDGE_RATE:-200}"
BRIDGE_DURATION="${KEEL_P6D_BRIDGE_DURATION:-60s}"
CHAOS_DURATION="${KEEL_P6D_CHAOS_DURATION:-10m}"
CACHE_GENERATIONS="${KEEL_P6D_CACHE_GENERATIONS:-10}"
CEILING_BYTES="${KEEL_P6D_CEILING_BYTES:-1932735283}" # 1.8 GiB
MANUAL_SLOW_MS="${KEEL_P6D_SLOW_MS:-15000}"
MANUAL_FAST_MS="${KEEL_P6D_FAST_MS:-50}"
RUN_TIMEOUT="${KEEL_P6D_RUN_TIMEOUT:-5m}"
BACKOFF_PROBE_AT="${KEEL_P6D_BACKOFF_PROBE_AT:-2m}"
PROBE_WINDOW="${KEEL_P6D_PROBE_WINDOW:-70s}"

PROVIDER_PORT="${KEEL_P6D_PROVIDER_PORT:-13688}"
MCP_PORT="${KEEL_P6D_MCP_PORT:-13689}"
SERVE_PORT="${KEEL_P6D_SERVE_PORT:-13750}"
PPROF_PORT="${KEEL_P6D_PPROF_PORT:-13751}"

NET_CTR=keel-p6d-net
MCP_CTR=keel-p6d-mcp
PROVIDER_CTR=keel-p6d-provider
SERVE_CTR=keel-p6d-serve
LOAD_CTR=keel-p6d-load

STAMP="$(date +%Y%m%d-%H%M%S)"
DB="keel_p6d_perf_${STAMP}"
STATE_DIR="$RUN_DIR/mcp-state"

log() { printf '[mcp-apps-perf] %s\n' "$*"; }
die() { printf '[mcp-apps-perf] FATAL: %s\n' "$*" >&2; exit 1; }

rm -rf "$RUN_DIR"
mkdir -p "$EVIDENCE" "$RUN_DIR" "$STATE_DIR"
# A fresh run must not inherit a previous PASS artifact.
rm -f "$EVIDENCE"/perf.json "$EVIDENCE"/summary.json "$EVIDENCE"/summary.txt \
      "$EVIDENCE"/heap-*.pb.gz "$EVIDENCE"/pprof-*.txt "$EVIDENCE"/heap-focus.json \
      "$EVIDENCE"/metrics-*.prom "$EVIDENCE"/catalog-*.json \
      "$EVIDENCE"/serve.log "$EVIDENCE"/mcp-server.log "$EVIDENCE"/provider.log \
      "$EVIDENCE"/load.log "$EVIDENCE"/gate.txt "$EVIDENCE"/run.log \
      "$EVIDENCE"/port-release.txt 2>/dev/null || true
exec > >(tee -a "$EVIDENCE/run.log") 2>&1

PG_CTR=""
CLEANED=0
cleanup() {
  local rc=$?
  if [ "$CLEANED" = "1" ]; then return; fi
  CLEANED=1
  trap - EXIT
  docker rm -f "$LOAD_CTR" "$SERVE_CTR" "$MCP_CTR" "$PROVIDER_CTR" "$NET_CTR" >/dev/null 2>&1 || true
  if [ -n "$PG_CTR" ]; then
    docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1 || true
  fi
  sleep 0.5
  {
    echo "# port release check (host; the fixture ports live in the removed container netns)"
    for p in "$PROVIDER_PORT" "$MCP_PORT" "$SERVE_PORT" "$PPROF_PORT"; do
      if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "port $p STILL LISTENING"
      else
        echo "port $p released (no host listener)"
      fi
    done
    echo "# containers: $(docker ps -a --format '{{.Names}}' | grep -c '^keel-p6d-' || true) remaining"
  } > "$EVIDENCE/port-release.txt" 2>&1 || true
  log "cleanup done (database $DB dropped, containers removed)"
  exit "$rc"
}
trap cleanup EXIT INT TERM

# --- prerequisites --------------------------------------------------------
command -v docker >/dev/null || die "docker is required"
command -v go >/dev/null || die "go is required"
docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
docker image inspect alpine:3.20 >/dev/null 2>&1 || die "alpine:3.20 image is required"

ARCH="$(uname -m)"
case "$ARCH" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64) GOARCH=amd64 ;;
  *) die "unsupported host architecture $ARCH" ;;
esac

ENV_FILE="$ROOT/.env.local"
[ -f "$ENV_FILE" ] || ENV_FILE="$ROOT/../../.env.personal"
[ -f "$ENV_FILE" ] || die "no env file found (.env.local or the main repo's .env.personal)"

# The DSN is read by Python only; it is never echoed or written to evidence.
DSN_LINE=""
while IFS= read -r line; do
  case "$line" in
    KEEL_TEST_DSN=*|KEEL_DSN=*) DSN_LINE="$line" ;;
  esac
done < <(grep -E '^KEEL_(TEST_)?DSN=' "$ENV_FILE" || true)
[ -n "$DSN_LINE" ] || die "KEEL_TEST_DSN/KEEL_DSN is not set in $ENV_FILE"

DSN_PORT="$(KEEL_DSN_LINE="$DSN_LINE" python3 - <<'PY'
import os, re
line = os.environ["KEEL_DSN_LINE"]
dsn = line.split("=", 1)[1].strip().strip('"').strip("'")
match = re.match(r"postgres(?:ql)?://([^@/]*@)?([^/:?]+)(?::(\d+))?", dsn)
print((match.group(3) if match and match.group(3) else "5432"))
PY
)"
PG_CTR="$(docker ps --format '{{.Names}}\t{{.Ports}}' | awk -F'\t' -v port=":${DSN_PORT}->" 'index($2, port) { print $1; exit }')"
[ -n "$PG_CTR" ] || die "no postgres container publishes port $DSN_PORT"
PG_INTERNAL_PORT="$(docker port "$PG_CTR" | awk -v hostport="$DSN_PORT" '{split($1,a,"/"); n=split($3,b,":"); if (b[n]==hostport) {print a[1]; exit}}')"
[ -n "$PG_INTERNAL_PORT" ] || PG_INTERNAL_PORT=5432
# Docker Desktop's DNS returns a host-side alias for container names; use the
# container's bridge IP directly so the private network namespace reaches the
# database without a host gateway mapping.
PG_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$PG_CTR" 2>/dev/null | head -c 64)"
if [ -z "$PG_IP" ]; then
  PG_IP=172.17.0.1
  PG_INTERNAL_PORT="$DSN_PORT"
fi
log "postgres container: $PG_CTR ($PG_IP:$PG_INTERNAL_PORT; host port $DSN_PORT), database $DB"

eval "$(KEEL_DSN_LINE="$DSN_LINE" DB_NAME="$DB" PG_HOST="$PG_IP" PG_PORT="$PG_INTERNAL_PORT" python3 - <<'PY'
import os, re, shlex
line = os.environ["KEEL_DSN_LINE"]
dsn = line.split("=", 1)[1].strip().strip('"').strip("'")
db = os.environ["DB_NAME"]
# Keep credentials/query; replace only the database name.
host_dsn = re.sub(r"/[^/?]+(\?|$)", "/" + db + r"\1", dsn, count=1)
# Inside the private network namespace the postgres container is reachable by
# its bridge IP on its internal port; no host gateway mapping is needed.
container_dsn = re.sub(r"@[^/:?]+(?::\d+)?",
                        "@" + os.environ["PG_HOST"] + ":" + os.environ["PG_PORT"],
                        host_dsn, count=1)
if "sslmode=" not in container_dsn:
    container_dsn += ("&" if "?" in container_dsn else "?") + "sslmode=disable"
print("export HOST_DSN=" + shlex.quote(host_dsn))
print("export CONTAINER_DSN=" + shlex.quote(container_dsn))
password = ""
try:
    from urllib.parse import urlsplit, unquote
    password = unquote(urlsplit(dsn).password or "")
except Exception:
    pass
print("export DB_PASSWORD=" + shlex.quote(password))
PY
)"
[ -n "${HOST_DSN:-}" ] || die "could not derive the host DSN"

# --- database + tenant ----------------------------------------------------
log "== fresh database $DB =="
docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null \
  || die "could not drop $DB"
docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE \"$DB\"" >/dev/null \
  || die "could not create $DB"

TENANT="$(python3 -c 'import uuid; print(uuid.uuid4())')"
log "== build binaries =="
(
  cd "$ROOT"
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -tags keelperf -o "$RUN_DIR/keel" ./cmd/keel
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$RUN_DIR/mcpappload" ./cmd/mcpappload
  go build -o "$RUN_DIR/keel-host" ./cmd/keel
)
log "creating tenant $TENANT (loopback-only local-admin auth: no account login in this lane)"
KEEL_DSN="$HOST_DSN" "$RUN_DIR/keel-host" admin tenant create --name p6d-perf --tenant "$TENANT" >/dev/null \
  || die "could not create the test tenant"
# The default tenant stream quota is 20; the G5 profile holds 256 control
# streams (the server's scheduler cap) across 1000 sessions.
KEEL_DSN="$HOST_DSN" "$RUN_DIR/keel-host" admin quota set --tenant "$TENANT" --streams 2000 >/dev/null \
  || die "could not raise the tenant stream quota"

cat > "$RUN_DIR/models.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "perf",
      "name": "G5 perf stub",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:${PROVIDER_PORT}/v1"},
      "access": {"type": "api-key", "env": "PERF_STUB_KEY"},
      "modelIds": ["perf-fast", "perf-slow"],
      "visibility": "visible"
    }
  ],
  "selection": {"providerId": "perf", "modelId": "perf-fast"}
}
JSON

cat > "$RUN_DIR/mcp.json" <<JSON
{"mcpServers":{"perf-fleet":{"url":"http://127.0.0.1:${MCP_PORT}/mcp","timeout":30}}}
JSON

# --- gate (build/tests used by this lane) --------------------------------
log "== gate: build, vet, cache-bound test =="
{
  echo "## go build ./..."
  ( cd "$ROOT" && go build ./... )
  echo "## go vet ./cmd/mcpappload"
  ( cd "$ROOT" && go vet ./cmd/mcpappload )
  echo "## go test ./internal/mcphost -run TestRegistryCacheBoundAtG5Scale"
  ( cd "$ROOT" && go test ./internal/mcphost -run TestRegistryCacheBoundAtG5Scale -count=1 -v )
  echo "## builds: linux/$GOARCH keel (tags keelperf), mcpappload; host keel"
  ls -l "$RUN_DIR/keel" "$RUN_DIR/mcpappload" "$RUN_DIR/keel-host"
} > "$EVIDENCE/gate.txt" 2>&1 || die "gate failed (see $EVIDENCE/gate.txt)"

# --- containers -----------------------------------------------------------
log "== start network holder + fixtures =="
docker rm -f "$NET_CTR" "$MCP_CTR" "$PROVIDER_CTR" "$SERVE_CTR" "$LOAD_CTR" >/dev/null 2>&1 || true
docker run -d --name "$NET_CTR" alpine:3.20 sleep 2147483647 >/dev/null
# The fixture runs as a child of a tiny shell, not as container PID 1: Linux
# gives PID 1 special signal semantics, and the chaos fixture must be able to
# SIGKILL itself so Docker's restart policy exercises the real reconnect path.
docker run -d --name "$MCP_CTR" --network "container:$NET_CTR" --restart=always \
  -v "$RUN_DIR:/run:ro" -v "$STATE_DIR:/state" \
  alpine:3.20 sh -c "/run/mcpappload mcpserver -addr 127.0.0.1:$MCP_PORT -apps $APPS -doc-bytes $DOC_BYTES -state /state/revision; exit 1" >/dev/null
docker run -d --name "$PROVIDER_CTR" --network "container:$NET_CTR" --restart=always \
  -v "$RUN_DIR:/run:ro" alpine:3.20 /run/mcpappload provider \
  -addr "127.0.0.1:$PROVIDER_PORT" -slow-ms "$MANUAL_SLOW_MS" -fast-ms "$MANUAL_FAST_MS" >/dev/null

wait_http() { # URL DESCRIPTION
  local url="$1" desc="$2"
  for _ in $(seq 1 300); do
    if docker exec "$NET_CTR" wget -qO- "$url" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  die "$desc did not become healthy"
}
wait_http "http://127.0.0.1:$MCP_PORT/healthz" "MCP fixture"
wait_http "http://127.0.0.1:$PROVIDER_PORT/healthz" "provider stub"

log "== start keel serve under --memory=2g =="
docker run -d --name "$SERVE_CTR" --network "container:$NET_CTR" \
  --memory=2g --memory-swap=2g --cpus=4 \
  -v "$RUN_DIR:/run:ro" \
  -e "KEEL_DSN=$CONTAINER_DSN" \
  -e "KEEL_MODELS_CONFIG=/run/models.json" \
  -e "KEEL_MCP_CONFIG=/run/mcp.json" \
  -e "KEEL_MCP_PROTOCOL_ERA=modern" \
  -e "KEEL_PPROF_ADDR=127.0.0.1:$PPROF_PORT" \
  -e "PERF_STUB_KEY=perf-stub-not-a-credential" \
  -e "GOMEMLIMIT=1610612736" \
  -e "KEEL_CAS_DIR=/tmp/cas" \
  -e "HOME=/tmp" \
  -e "KEEL_PLUGINS_ROOT=/tmp/plugins" \
  alpine:3.20 /run/keel serve \
  --addr "127.0.0.1:$SERVE_PORT" --auth local-only --tenant "$TENANT" --runs "$RUN_SLOTS" \
  --web-dir /tmp/web --cas /tmp/cas \
  --mcp /run/mcp.json --models-config /run/models.json >/dev/null

wait_http "http://127.0.0.1:$SERVE_PORT/v1/health" "keel serve"
wait_http "http://127.0.0.1:$PPROF_PORT/debug/pprof/" "pprof seam"

# The catalogue must be published before the load starts; otherwise the scale
# snapshot would race the first fetch.
log "waiting for the app catalogue"
CATALOG_OK=0
for _ in $(seq 1 600); do
  BODY="$(docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/v1/mcp/apps" 2>/dev/null || true)"
  COUNT="$(BODY="$BODY" APPS="$APPS" python3 - <<'PY' 2>/dev/null || echo 0
import json, os
try:
    doc = json.loads(os.environ.get("BODY") or "{}")
    print(len(doc.get("apps") or []))
except Exception:
    print(0)
PY
)"
  if [ "$COUNT" = "$APPS" ]; then CATALOG_OK=1; break; fi
  sleep 0.2
done
[ "$CATALOG_OK" = "1" ] || die "app catalogue did not publish $APPS apps (see serve logs)"
docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/v1/mcp/apps" > "$EVIDENCE/catalog-boot.json" 2>/dev/null || true
docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/metrics" > "$EVIDENCE/metrics-boot.prom" 2>/dev/null || true

log "== load: $SESSIONS sessions / $RUN_SLOTS runs / $BRIDGE_RATE fps / chaos $CHAOS_DURATION =="
set +e
docker run --rm --name "$LOAD_CTR" --network "container:$NET_CTR" \
  -v "$RUN_DIR:/run:ro" -v "$EVIDENCE:/ev" \
  alpine:3.20 /run/mcpappload load \
  -base "http://127.0.0.1:$SERVE_PORT" \
  -pprof "http://127.0.0.1:$PPROF_PORT" \
  -control "http://127.0.0.1:$MCP_PORT" \
  -out /ev/perf.json -evidence /ev \
  -sessions "$SESSIONS" -first-batch "$FIRST_BATCH" -sse-cap "$SSE_CAP" \
  -apps "$APPS" -run-slots "$RUN_SLOTS" \
  -bridge-rate "$BRIDGE_RATE" -bridge-duration "$BRIDGE_DURATION" \
  -chaos-duration "$CHAOS_DURATION" -cache-generations "$CACHE_GENERATIONS" \
  -run-timeout "$RUN_TIMEOUT" -backoff-probe-at "$BACKOFF_PROBE_AT" -probe-window "$PROBE_WINDOW" \
  > "$EVIDENCE/load.log" 2>&1
LOAD_RC=$?
set -e
log "load exit code: $LOAD_RC"

# --- evidence collection --------------------------------------------------
log "== collect evidence =="
docker logs "$SERVE_CTR" > "$EVIDENCE/serve.log" 2>&1 || true
docker logs "$MCP_CTR" > "$EVIDENCE/mcp-server.log" 2>&1 || true
docker logs "$PROVIDER_CTR" > "$EVIDENCE/provider.log" 2>&1 || true
docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/metrics" > "$EVIDENCE/metrics-final.prom" 2>/dev/null || true
docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/v1/mcp/apps" > "$EVIDENCE/catalog-final.json" 2>/dev/null || true
docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/v1/mcp/servers" > "$EVIDENCE/mcp-servers-final.json" 2>/dev/null || true
docker exec "$NET_CTR" wget -qO- "http://127.0.0.1:$SERVE_PORT/v1/mcp/apps/audit?limit=200" > "$EVIDENCE/audit-final.json" 2>/dev/null || true

PEAK_BYTES=""
for _ in $(seq 1 20); do
  PEAK_BYTES="$(docker exec "$SERVE_CTR" cat /sys/fs/cgroup/memory.peak 2>/dev/null | tr -d '\r' || true)"
  case "$PEAK_BYTES" in ''|*[!0-9]*) PEAK_BYTES=""; sleep 0.2 ;; *) break ;; esac
done
if [ -z "$PEAK_BYTES" ]; then
  log "cgroup memory.peak unreadable; failing closed"
  printf '{\n  "mode": "mcp-apps-perf",\n  "verdict": "FAIL",\n  "failures": ["cgroup memory.peak unreadable"],\n  "loadExitCode": %s\n}\n' "$LOAD_RC" > "$EVIDENCE/summary.json"
  exit 1
fi
log "cgroup memory.peak: $PEAK_BYTES bytes ($(python3 -c "print(f'{int($PEAK_BYTES)/2**20:.1f}')") MiB)"

# --- heap-focus extraction ------------------------------------------------
HEAP_FOCUS="$EVIDENCE/heap-focus.json"
python3 - "$EVIDENCE" "$HEAP_FOCUS" >/dev/null 2>&1 <<'PY' || true
import json, os, re, subprocess, sys
evidence, out = sys.argv[1], sys.argv[2]

def parse_top(path):
    text = open(path, encoding="utf-8", errors="replace").read()
    match = re.search(r"Showing nodes accounting for\s+([0-9.]+)\s*([kKMGT]?B)", text)
    if match:
        value, unit = float(match.group(1)), match.group(2)
        scale = {"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
                 "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3}[unit]
        return value * scale
    total = 0.0
    pattern = re.compile(r"^\s*([0-9.]+)\s*([kKMGT]?B)\s+[0-9.]+[kKMGT]?B\s+.*mcphost", re.M)
    for value, unit in pattern.findall(text):
        total += float(value) * {"B": 1, "kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
                                 "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3}[unit]
    return total

result = {}
for name in ("100", "1000", "runs", "generations"):
    profile = os.path.join(evidence, f"heap-{name}.pb.gz")
    text_path = os.path.join(evidence, f"pprof-{name}.txt")
    if not os.path.exists(profile):
        result[name] = {"error": "profile missing"}
        continue
    command = ["go", "tool", "pprof", "-top", "-sample_index=inuse_space",
               "-focus=jixoai/keel/internal/mcphost", "-nodecount=40", profile]
    with open(text_path, "w", encoding="utf-8") as fh:
        proc = subprocess.run(command, stdout=fh, stderr=subprocess.STDOUT, text=True)
    if proc.returncode != 0:
        result[name] = {"error": f"pprof exit {proc.returncode}"}
        continue
    focus = parse_top(text_path)
    result[name] = {"bytes": int(focus), "mb": round(focus / 1e6, 3)}
json.dump(result, open(out, "w"), indent=2)
PY

# --- summary + verdict ----------------------------------------------------
set +e
python3 - "$EVIDENCE" "$PEAK_BYTES" "$CEILING_BYTES" "$LOAD_RC" "$RUN_SLOTS" "$APPS" \
  "$SESSIONS" "$BRIDGE_RATE" "$BRIDGE_DURATION" "$CHAOS_DURATION" "$SSE_CAP" <<'PY'
import json, os, sys

evidence, peak_raw, ceiling_raw, load_rc, run_slots, apps, sessions, bridge_rate, bridge_duration, chaos_duration, sse_cap = sys.argv[1:12]
peak = int(peak_raw) if peak_raw.isdigit() else None
ceiling = int(ceiling_raw)
run_slots = int(run_slots); apps = int(apps); sessions = int(sessions); sse_cap = int(sse_cap)
load_rc = int(load_rc)

def seconds(value):
    value = str(value).strip()
    if value.endswith("ms"):
        return float(value[:-2]) / 1000.0
    if value.endswith("s"):
        return float(value[:-1])
    if value.endswith("m"):
        return float(value[:-1]) * 60.0
    if value.endswith("h"):
        return float(value[:-1]) * 3600.0
    return float(value)

bridge_seconds = seconds(bridge_duration)
chaos_seconds = seconds(chaos_duration)
failures = []

def read_json(name):
    path = os.path.join(evidence, name)
    try:
        with open(path) as fh:
            return json.load(fh)
    except Exception as exc:
        failures.append(f"{name} unreadable: {exc}")
        return {}

perf = read_json("perf.json")
heap = read_json("heap-focus.json")

if load_rc != 0:
    failures.append(f"load tool exited {load_rc}")
for item in perf.get("failures") or []:
    failures.append("load: " + item)
if perf.get("sessions") != sessions:
    failures.append(f"sessions = {perf.get('sessions')}, want {sessions}")
if perf.get("apps") != apps:
    failures.append(f"apps = {perf.get('apps')}, want {apps}")
if perf.get("sseStreams", 0) < min(sessions, sse_cap):
    failures.append(f"sseStreams = {perf.get('sseStreams')}, want >= {min(sessions,sse_cap)}")

runs = perf.get("runs") or {}
if runs.get("maxActive") != run_slots:
    failures.append(f"max concurrent runs = {runs.get('maxActive')}, want {run_slots}")
if runs.get("finishedDone", 0) < sessions:
    failures.append(f"runs finished = {runs.get('finishedDone')}, want {sessions}")
if runs.get("finishedFailed", 0):
    failures.append(f"runs failed = {runs.get('finishedFailed')}")

catalog = perf.get("catalog") or {}
if not catalog.get("identical"):
    failures.append("the app catalogue is not identical at 100 vs 1000 sessions")
if (catalog.get("at100") or {}).get("apps") != apps or (catalog.get("at1000") or {}).get("apps") != apps:
    failures.append("catalogue app count mismatch at a scale checkpoint")
if (catalog.get("atRuns") or {}).get("apps") != apps:
    failures.append("catalogue app count mismatch under the 64-run load")
if (catalog.get("atRuns") or {}).get("setHash") != (catalog.get("at100") or {}).get("setHash"):
    failures.append("the app catalogue changed under the 64-run load")

metrics = perf.get("metrics") or {}
if not metrics.get("seriesStable"):
    failures.append("the mcp_* series set changed between 100 and 1000 sessions")
for family in ("mcp_servers", "mcp_surface_generation", "mcp_app_fetch_total",
               "mcp_app_bridge_frames_total", "mcp_listen_reconnects_total", "mcp_mrtr_rounds"):
    if (metrics.get("familyCounts") or {}).get(family, 0) == 0:
        failures.append(f"metric family {family} is absent")
if sum((metrics.get("familyCounts") or {}).values()) > 40:
    failures.append(f"mcp_* series count grew to {sum((metrics.get('familyCounts') or {}).values())}, want <= 40")

focus100 = (heap.get("100") or {}).get("mb")
focus1000 = (heap.get("1000") or {}).get("mb")
focus_runs = (heap.get("runs") or {}).get("mb")
focus_gen = (heap.get("generations") or {}).get("mb")
if focus100 is None or focus1000 is None:
    failures.append("heap focus at 100/1000 sessions is unreadable")
else:
    delta = abs((focus1000 or 0) - (focus100 or 0))
    tolerance = max(8.0, 0.10 * max(focus100 or 0, 1.0))
    if delta > tolerance:
        failures.append(f"mcphost heap grew {delta:.1f} MB between 100 and 1000 sessions (tolerance {tolerance:.1f} MB)")
if focus_runs is None:
    failures.append("64-run heap focus is unreadable")
elif focus_runs > 80.0:
    failures.append(f"mcphost heap under the run load = {focus_runs:.1f} MB, over the 64 MiB LRU bound + 25% sampling allowance")
if focus_gen is None:
    failures.append("post-generation heap focus is unreadable")
elif focus_gen > 80.0:
    failures.append(f"post-generation mcphost heap = {focus_gen:.1f} MB, over the 64 MiB LRU bound + 25% sampling allowance")

bridge = perf.get("bridge") or {}
if bridge.get("status429", 0):
    failures.append(f"normal bridge load saw {bridge.get('status429')} 429s")
if bridge.get("errors", 0) or bridge.get("other", 0):
    failures.append(f"normal bridge load errors={bridge.get('errors')} other={bridge.get('other')}")
if float(bridge.get("durationSec", 0)) < 0.9 * bridge_seconds:
    failures.append(f"bridge duration {bridge.get('durationSec')}s is short")
if bridge.get("requests", 0) < int(0.9 * int(bridge_rate) * bridge_seconds):
    failures.append(f"bridge requests = {bridge.get('requests')}, below the expected rate budget")

pre = perf.get("preLimit") or {}
if pre.get("bridgeRateRefusals", 0) or pre.get("concurrencyRefusals", 0):
    failures.append("normal load produced refusal audit rows")
over = perf.get("overLimit") or {}
if over.get("rate429", 0) <= 0:
    failures.append("the deliberate rate burst produced no 429")
if over.get("rateAuditRows", 0) <= 0:
    failures.append("the deliberate rate refusal produced no audit row")
if over.get("concurrency429", 0) <= 0:
    failures.append("five concurrent slow calls produced no concurrency 429")
if over.get("concurrencyOK") != 4:
    failures.append(f"concurrency probe admitted {over.get('concurrencyOK')} calls, want 4")
if over.get("concurrencyAuditRows", 0) <= 0:
    failures.append("the deliberate concurrency refusal produced no audit row")

chaos = perf.get("chaos") or {}
if chaos_seconds >= 600:
    if chaos.get("kills", 0) < 8:
        failures.append(f"chaos kills = {chaos.get('kills')}, want >= 8")
    if chaos.get("drops", 0) < 8:
        failures.append(f"chaos drops = {chaos.get('drops')}, want >= 8")
    if chaos.get("maxRecoveryMs", 0) > 35000:
        failures.append(f"max recovery = {chaos.get('maxRecoveryMs')}ms, want <= 35000ms")
    if chaos.get("maxDropRecoveryMs", 0) > 10000:
        failures.append(f"max drop recovery = {chaos.get('maxDropRecoveryMs')}ms, want <= 10000ms")
    if chaos.get("maxListenAttemptGapMs", 0) > 35000:
        failures.append(f"max listen attempt gap = {chaos.get('maxListenAttemptGapMs')}ms, want <= 35000ms")
    if chaos.get("healthFailures", 0) > 0:
        failures.append(f"health failures during chaos = {chaos.get('healthFailures')}")
    if chaos.get("pidChanges", 0) > 0:
        failures.append(f"runtime process changed during chaos: {chaos.get('pidChanges')}")
    if chaos.get("catalogBadSamples", 0) > 0:
        failures.append(f"catalogue incomplete in {chaos.get('catalogBadSamples')} chaos samples")
    if chaos.get("catalogHashChanges", 0) < chaos.get("kills", 0):
        failures.append(f"catalogue hash changes {chaos.get('catalogHashChanges')} < kills {chaos.get('kills')}")
    if chaos.get("auditSubscriptionRows", 0) <= 0:
        failures.append("no subscription.listen audit rows after chaos")
    if chaos.get("finalApps") != apps:
        failures.append(f"final chaos catalogue has {chaos.get('finalApps')} apps")
else:
    # A short smoke run is not a G5 certification; it must never be reported as
    # PASS. The script exists for the full 10-minute window.
    failures.append("chaos duration is shorter than 10 minutes: a smoke run cannot certify G5")

final = perf.get("final") or {}
if (final.get("catalog") or {}).get("apps") != apps:
    failures.append("final catalogue app count mismatch")
if sum([final.get("auditOk", 0), final.get("auditClosed", 0), final.get("auditError", 0)]) <= 0:
    failures.append("no subscription.listen audit evidence in the final audit sample")

if peak is None:
    failures.append("cgroup memory.peak unreadable")
elif peak >= ceiling:
    failures.append(f"cgroup peak {peak} bytes is not below the 1.8 GiB ceiling {ceiling}")

summary = {
    "mode": "mcp-apps-perf",
    "lane": "P6D",
    "verdict": "FAIL" if failures else "PASS",
    "sessions": perf.get("sessions"),
    "sseStreams": perf.get("sseStreams"),
    "runs": runs,
    "apps": perf.get("apps"),
    "catalog": {
        "identical": catalog.get("identical"),
        "bytesAt100": (catalog.get("at100") or {}).get("canonicalBytes"),
        "bytesAt1000": (catalog.get("at1000") or {}).get("canonicalBytes"),
        "hashAt100": (catalog.get("at100") or {}).get("setHash"),
        "hashAt1000": (catalog.get("at1000") or {}).get("setHash"),
        "responseBytesAt100": (catalog.get("at100") or {}).get("responseBytes"),
        "responseBytesAt1000": (catalog.get("at1000") or {}).get("responseBytes"),
    },
    "heap": {
        "mcphostAt100MB": focus100,
        "mcphostAt1000MB": focus1000,
        "mcphostDeltaMB": None if focus100 is None or focus1000 is None else round(abs(focus1000 - focus100), 3),
        "mcphostAtRunsMB": focus_runs,
        "mcphostAfterGenerationsMB": focus_gen,
        "cacheBoundMiB": 64,
    },
    "bridge": bridge,
    "overLimit": over,
    "preLimit": pre,
    "chaos": chaos,
    "metrics": {
        "seriesAt100": metrics.get("seriesAt100"),
        "seriesAt1000": metrics.get("seriesAt1000"),
        "seriesStable": metrics.get("seriesStable"),
        "familyCounts": metrics.get("familyCounts"),
        "finalHeapAllocMB": (perf.get("final") or {}).get("metrics", {}).get("finalHeapAllocMB"),
    },
    "memory": {
        "cgroupPeakBytes": peak,
        "cgroupPeakMB": None if peak is None else round(peak / 1e6, 1),
        "cgroupPeakMiB": None if peak is None else round(peak / 2**20, 1),
        "ceilingBytes": ceiling,
        "ceilingMiB": round(ceiling / 2**20, 1),
    },
    "failures": failures,
    "loadExitCode": load_rc,
}
json.dump(summary, open(os.path.join(evidence, "summary.json"), "w"), indent=2)
open(os.path.join(evidence, "summary.json"), "a").write("\n")

lines = [
    "MCP Apps G5 acceptance (P6D) — scale / 2GB / chaos",
    "=" * 62,
    f"verdict: {'PASS' if not failures else 'FAIL'}",
    f"sessions: {perf.get('sessions')} (SSE streams: {perf.get('sseStreams')})",
    f"runs: maxActive={runs.get('maxActive')} finished={runs.get('finishedDone')} failed={runs.get('finishedFailed')} (slots {run_slots})",
    f"apps: {perf.get('apps')}  catalogue bytes { (catalog.get('at100') or {}).get('canonicalBytes') } vs { (catalog.get('at1000') or {}).get('canonicalBytes') }  identical={catalog.get('identical')}",
    f"mcphost heap: {focus100} MB @100 / {focus1000} MB @1000 / {focus_runs} MB @64 runs / {focus_gen} MB after generations",
    f"bridge: {bridge.get('requests')} req in {bridge.get('durationSec')}s, ok={bridge.get('ok')} 429={bridge.get('status429')} errors={bridge.get('errors')} p99={bridge.get('p99Ms')}ms",
    f"over-limit: rate429={over.get('rate429')} concurrency429={over.get('concurrency429')} audit={over.get('rateAuditRows')}/{over.get('concurrencyAuditRows')}",
    f"chaos: kills={chaos.get('kills')} drops={chaos.get('drops')} reconnects={chaos.get('reconnectsDelta')} maxGap={chaos.get('maxListenAttemptGapMs')}ms maxRecovery={chaos.get('maxRecoveryMs')}ms",
    f"memory: cgroup peak {round((peak or 0)/2**20,1)} MiB / {round(ceiling/2**20,1)} MiB ceiling",
    f"metrics: series {metrics.get('seriesAt100')} vs {metrics.get('seriesAt1000')} stable={metrics.get('seriesStable')} families={metrics.get('familyCounts')}",
]
if failures:
    lines.append("")
    lines.append("failures:")
    lines += ["  - " + item for item in failures]
open(os.path.join(evidence, "summary.txt"), "w").write("\n".join(lines) + "\n")
print("\n".join(lines))
sys.exit(0 if not failures else 1)
PY
SUMMARY_RC=$?
set -e

# --- credential scan ------------------------------------------------------
if [ -n "${DB_PASSWORD:-}" ]; then
  python3 - "$EVIDENCE" "$DB_PASSWORD" <<'PY'
import os, sys
evidence, password = sys.argv[1], sys.argv[2]
leaks = []
for name in sorted(os.listdir(evidence)):
    path = os.path.join(evidence, name)
    if not os.path.isfile(path) or os.path.getsize(path) > 8 << 20:
        continue
    try:
        with open(path, "rb") as fh:
            raw = fh.read()
        if password.encode() in raw:
            with open(path, "wb") as fh:
                fh.write(raw.replace(password.encode(), b"[REDACTED]"))
            leaks.append(name)
    except Exception:
        continue
if leaks:
    print("REDACTED credential material from: " + ", ".join(leaks), file=sys.stderr)
    sys.exit(1)
PY
  SCAN_RC=$?
  set -e
  if [ "$SCAN_RC" -ne 0 ]; then
    log "a credential was found in evidence and redacted; forcing FAIL"
    SUMMARY_RC=1
  fi
fi

if [ "$SUMMARY_RC" = "0" ]; then
  log "verdict: PASS"
else
  log "verdict: FAIL (see $EVIDENCE/summary.json)"
fi
log "evidence: $EVIDENCE"
exit "$SUMMARY_RC"
