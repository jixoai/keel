#!/usr/bin/env bash
# External gateway smoke: build the runtime and the reference DSH sidecar,
# start a real runtime on a real database, open an external session through the
# HTTP API, send a message, replay the projected events through the ordinary
# SSE/event endpoints, close it, and prove the whole process tree is reaped.
#
# Usage:
#   scripts/external-gateway-smoke.sh
#
# Environment:
#   KEEL_DSN          optional; default creates/uses the local docker PostgreSQL
#                     (dockerized Postgres named $KEEL_PG_CTR, port 13741) and the
#                     dedicated database keel_gw_smoke. A custom DSN is only
#                     accepted when its database name contains "smoke" (the
#                     script truncates and drops it).
#   KEEL_SMOKE_PORT   optional; default 13796.
#   KEEL_PG_CTR       optional; name of the dockerized Postgres container
#                     (default keel-pg).
#
# The script fails on the first mismatch and always cleans up (runtime process,
# sidecar/process group, database, temp dir). Every step prints one PASS line
# with the observed value so the output can be pasted into a report.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/keel-gw-smoke.XXXXXX)"
PORT="${KEEL_SMOKE_PORT:-13796}"
DB="keel_gw_smoke"
PG_CTR="${KEEL_PG_CTR:-keel-pg}"
TENANT="11111111-1111-4111-8111-111111111111"
ADMIN_KEY="smoke-admin-$(date +%s)-$$"
RUNTIME_PID=""
CHILD_PID=""
SSE_PID=""
USING_DOCKER=0
STEP=0

log() { printf '%s\n' "$*"; }
step() { STEP=$((STEP + 1)); log "[$STEP] $*"; }
pass() { log "PASS [$STEP] $*"; }
fail() { log "FAIL [$STEP] $*"; exit 1; }

json_get() {
  python3 -c '
import json,sys
try:
    data=json.load(open(sys.argv[1]))
except Exception as exc:
    sys.stderr.write("json: %s\n" % exc)
    sys.exit(2)
cur=data
for part in sys.argv[2].split("."):
    if part == "": continue
    if isinstance(cur,list):
        cur=cur[int(part)]
    else:
        cur=cur.get(part) if isinstance(cur,dict) else None
    if cur is None:
        print("")
        sys.exit(0)
print(cur if not isinstance(cur,(dict,list)) else json.dumps(cur))
' "$1" "$2"
}

cleanup() {
  local code=$?
  if [[ -n "${SSE_PID}" ]] && kill -0 "${SSE_PID}" 2>/dev/null; then
    kill "${SSE_PID}" 2>/dev/null || true
  fi
  if [[ -n "${RUNTIME_PID}" ]] && kill -0 "${RUNTIME_PID}" 2>/dev/null; then
    kill -TERM "${RUNTIME_PID}" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "${RUNTIME_PID}" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "${RUNTIME_PID}" 2>/dev/null || true
  fi
  if [[ "${USING_DOCKER}" == "1" ]]; then
    docker exec "${PG_CTR}" psql -U postgres -d postgres -c \
      "DROP DATABASE IF EXISTS ${DB} WITH (FORCE)" >/dev/null 2>&1 || true
  fi
  rm -rf "${TMP}"
  exit "${code}"
}
trap cleanup EXIT

if lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
  fail "port ${PORT} is already in use"
fi

step "build runtime and reference sidecar"
mkdir -p "${TMP}/bin"
(cd "${ROOT}" && go build -o "${TMP}/bin/keel-runtime" ./cmd/runtime)
(cd "${ROOT}" && go build -o "${TMP}/bin/keel-dsh-ref" ./cmd/keel-dsh-ref)
pass "binaries built in ${TMP}/bin"

step "prepare a dedicated database (${DB})"
if [[ -n "${KEEL_DSN:-}" ]]; then
  case "${KEEL_DSN}" in
    *smoke*) ;;
    *) fail "KEEL_DSN must name a smoke database (got a DSN without 'smoke')" ;;
  esac
  DSN="${KEEL_DSN}"
else
  if ! docker ps --format '{{.Names}}' | grep -qx "${PG_CTR}"; then
    fail "docker container ${PG_CTR} is not running (start it, set KEEL_PG_CTR, or set KEEL_DSN)"
  fi
  USING_DOCKER=1
  docker exec "${PG_CTR}" psql -U postgres -d postgres -c \
    "DROP DATABASE IF EXISTS ${DB} WITH (FORCE)" >/dev/null
  docker exec "${PG_CTR}" psql -U postgres -d postgres -c \
    "CREATE DATABASE ${DB}" >/dev/null
  DSN="postgres://postgres:runtime@127.0.0.1:13741/${DB}?sslmode=disable"
fi
pass "database ready"

step "write the external provider declarations (one host.TenantConfig vocabulary)"
cat >"${TMP}/external.json" <<JSON
{
  "external": {
    "backends": {
      "dsh-ref": {
        "command": "${TMP}/bin/keel-dsh-ref",
        "args": ["-usage", "-prompt=smoke:", "-spawn-child"],
        "timeoutMs": 5000,
        "maxSessions": 4
      }
    },
    "tools": {
      "dsh-tools": {
        "command": "${TMP}/bin/keel-dsh-ref",
        "args": ["-kind=tool"]
      }
    },
    "quota": {"unmetered": "allow", "maxSessionsPerBackend": 8}
  }
}
JSON
pass "config written to ${TMP}/external.json"

step "start the runtime on 127.0.0.1:${PORT}"
KEEL_DSN="${DSN}" KEEL_ADMIN_KEY="${ADMIN_KEY}" \
KEEL_EXTERNAL_CONFIG="${TMP}/external.json" KEEL_RUNTIME_ID="smoke-$$" \
  "${TMP}/bin/keel-runtime" -addr "127.0.0.1:${PORT}" -no-worker \
  >"${TMP}/runtime.log" 2>&1 &
RUNTIME_PID=$!
ready=0
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:${PORT}/v1/ready" >/dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "${RUNTIME_PID}" 2>/dev/null; then fail "runtime exited during boot (see ${TMP}/runtime.log)"; fi
  sleep 0.1
done
[[ "${ready}" == "1" ]] || fail "runtime did not become ready"
pass "runtime ready (pid ${RUNTIME_PID})"

step "wait for the external agent backend to connect"
states=""
for _ in $(seq 1 100); do
  assert_file="${TMP}/backends.json"
  curl -fsS "http://127.0.0.1:${PORT}/v1/external/backends" -H "X-Tenant-ID: ${TENANT}" >"${assert_file}"
  states="$(json_get "${assert_file}" 'backends.0.state')"
  [[ "${states}" == "ready" ]] && break
  sleep 0.1
done
[[ "${states}" == "ready" ]] || fail "backend not ready: $(cat "${TMP}/backends.json")"
pass "backend ready"

CHILD_PID="$(grep -o 'spawned child pid=[0-9]*' "${TMP}/runtime.log" | head -1 | cut -d= -f2 || true)"
[[ -n "${CHILD_PID}" ]] || fail "sidecar did not report its grandchild pid"
kill -0 "${CHILD_PID}" 2>/dev/null || fail "sidecar grandchild ${CHILD_PID} is not alive"
pass "sidecar grandchild alive (pid ${CHILD_PID})"

step "create the tenant"
curl -fsS -X POST "http://127.0.0.1:${PORT}/v1/tenants" \
  -H "Authorization: Bearer ${ADMIN_KEY}" -H "X-Tenant-ID: ${TENANT}" \
  -H "Content-Type: application/json" -d '{"name":"smoke"}' >"${TMP}/tenant.json"
pass "tenant ${TENANT}"

step "open an external session"
curl -fsS -X POST "http://127.0.0.1:${PORT}/v1/external/sessions" \
  -H "X-Tenant-ID: ${TENANT}" -H "Content-Type: application/json" \
  -d '{"backend":"dsh-ref","reference":"smoke-1"}' >"${TMP}/open.json"
SESSION_ID="$(json_get "${TMP}/open.json" 'sessionId')"
[[ -n "${SESSION_ID}" ]] || fail "open returned no sessionId"
[[ "$(json_get "${TMP}/open.json" 'stateOwner')" == "external" ]] || fail "stateOwner != external"
[[ "$(json_get "${TMP}/open.json" 'externalStatus')" == "active" ]] || fail "externalStatus != active"
[[ "$(json_get "${TMP}/open.json" 'externalBackend')" == "dsh-ref" ]] || fail "externalBackend mismatch"
[[ "$(json_get "${TMP}/open.json" 'compatible')" == "True" ]] || fail "compatible marker missing"
[[ "$(json_get "${TMP}/open.json" 'nativeRecovery')" == "False" ]] || fail "nativeRecovery must be false"
pass "session ${SESSION_ID} (ref $(json_get "${TMP}/open.json" 'externalRef'), metering $(json_get "${TMP}/open.json" 'metering'))"

step "the native input endpoint refuses the external session (409, no event)"
code="$(curl -s -o "${TMP}/input.json" -w '%{http_code}' -X POST \
  "http://127.0.0.1:${PORT}/v1/sessions/${SESSION_ID}/input" \
  -H "X-Tenant-ID: ${TENANT}" -H "Content-Type: application/json" \
  -d '{"text":"native","requestId":"smoke-native"}')"
[[ "${code}" == "409" ]] || fail "native /input answered ${code}: $(cat "${TMP}/input.json")"
pass "native /input -> 409"

step "send a message to the backend"
curl -fsS -X POST "http://127.0.0.1:${PORT}/v1/external/sessions/${SESSION_ID}/messages" \
  -H "X-Tenant-ID: ${TENANT}" -H "Content-Type: application/json" \
  -d '{"text":"hello smoke","requestId":"smoke-send-1"}' >"${TMP}/send.json"
TURN_ID="$(json_get "${TMP}/send.json" 'turnId')"
[[ -n "${TURN_ID}" ]] || fail "send returned no turnId"
pass "message accepted (turn ${TURN_ID})"

step "replay the projected events through the ordinary event endpoint"
found_assistant=0
found_send=0
usage_seen=0
for _ in $(seq 1 100); do
  curl -fsS "http://127.0.0.1:${PORT}/v1/sessions/${SESSION_ID}/events" \
    -H "X-Tenant-ID: ${TENANT}" >"${TMP}/events.json"
  found_assistant="$(python3 -c '
import json
data=json.load(open("'"${TMP}"'/events.json"))
n=0
for ev in data.get("events",[]):
    if ev.get("kind")!="external.event": continue
    p=ev.get("payload",{})
    if p.get("direction")=="receive" and p.get("type")=="assistant.message": n+=1
print(n)
')"
  found_send="$(python3 -c '
import json
data=json.load(open("'"${TMP}"'/events.json"))
n=0
for ev in data.get("events",[]):
    p=ev.get("payload",{})
    if ev.get("kind")=="external.event" and p.get("direction")=="send": n+=1
print(n)
')"
  usage_seen="$(python3 -c '
import json
data=json.load(open("'"${TMP}"'/events.json"))
n=0
for ev in data.get("events",[]):
    p=ev.get("payload",{})
    if ev.get("kind")=="external.event" and p.get("usage"): n+=1
print(n)
')"
  if [[ "${found_assistant}" -ge 1 && "${found_send}" -ge 1 && "${usage_seen}" -ge 1 ]]; then break; fi
  sleep 0.1
done
[[ "${found_assistant}" -ge 1 ]] || fail "no assistant.message projection: $(cat "${TMP}/events.json")"
[[ "${found_send}" -ge 1 ]] || fail "no outgoing send record"
[[ "${usage_seen}" -ge 1 ]] || fail "no usage record on a metering=external backend"
pass "events projected (assistant=${found_assistant}, send=${found_send}, usage=${usage_seen})"

step "query the session (compatibility markers and usage)"
curl -fsS "http://127.0.0.1:${PORT}/v1/external/sessions/${SESSION_ID}" \
  -H "X-Tenant-ID: ${TENANT}" >"${TMP}/query.json"
[[ "$(json_get "${TMP}/query.json" 'externalStatus')" == "active" ]] || fail "query status != active"
total="$(json_get "${TMP}/query.json" 'usage.totalTokens')"
[[ -n "${total}" && "${total}" != "0" ]] || fail "query usage.totalTokens = ${total:-empty}, want a reported total"
pass "query active, usage.totalTokens=${total}"

step "SSE replay: the ordinary stream only carries Keel-recorded external.event frames"
curl -sN --max-time 5 "http://127.0.0.1:${PORT}/v1/sessions/${SESSION_ID}/events/stream" \
  -H "X-Tenant-ID: ${TENANT}" >"${TMP}/sse.txt" &
SSE_PID=$!
for _ in $(seq 1 50); do
  if grep -q 'event: external.event' "${TMP}/sse.txt" 2>/dev/null &&
     grep -Eq '"type": ?"assistant.message"' "${TMP}/sse.txt" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
kill "${SSE_PID}" 2>/dev/null || true
wait "${SSE_PID}" 2>/dev/null || true
grep -q 'event: external.event' "${TMP}/sse.txt" || fail "SSE did not replay the external.event: $(head -5 "${TMP}/sse.txt")"
grep -Eq '"type": ?"assistant.message"' "${TMP}/sse.txt" || fail "SSE replay lacks the assistant.message projection"
grep -Eq '"stateOwner": ?"external"' "${TMP}/sse.txt" || fail "SSE frame lacks the compatibility markers"
pass "SSE replayed $(grep -c 'event: external.event' "${TMP}/sse.txt") external.event frame(s) with markers"

step "cancel the turn (idempotent control path)"
curl -fsS -X POST "http://127.0.0.1:${PORT}/v1/external/sessions/${SESSION_ID}/cancel" \
  -H "X-Tenant-ID: ${TENANT}" -H "Content-Type: application/json" \
  -d "{\"turnId\":\"${TURN_ID}\",\"reason\":\"smoke\"}" >"${TMP}/cancel.json"
[[ "$(json_get "${TMP}/cancel.json" 'canceled')" == "True" ]] || fail "cancel did not answer"
pass "cancel answered"

step "close the session and refuse further sends"
curl -fsS -X POST "http://127.0.0.1:${PORT}/v1/external/sessions/${SESSION_ID}/close" \
  -H "X-Tenant-ID: ${TENANT}" -H "Content-Type: application/json" -d '{"reason":"smoke"}' >/dev/null
curl -fsS "http://127.0.0.1:${PORT}/v1/external/sessions/${SESSION_ID}" \
  -H "X-Tenant-ID: ${TENANT}" >"${TMP}/closed.json"
[[ "$(json_get "${TMP}/closed.json" 'externalStatus')" == "closed" ]] || fail "status after close != closed"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "http://127.0.0.1:${PORT}/v1/external/sessions/${SESSION_ID}/messages" \
  -H "X-Tenant-ID: ${TENANT}" -H "Content-Type: application/json" -d '{"text":"again"}')"
[[ "${code}" == "409" ]] || fail "send after close answered ${code}, want 409"
pass "closed and further sends -> 409"

step "stop the runtime and reap the whole process tree"
kill -TERM "${RUNTIME_PID}"
for _ in $(seq 1 100); do kill -0 "${RUNTIME_PID}" 2>/dev/null || break; sleep 0.1; done
kill -0 "${RUNTIME_PID}" 2>/dev/null && fail "runtime did not exit on SIGTERM"
RUNTIME_PID=""
for _ in $(seq 1 100); do
  if ! kill -0 "${CHILD_PID}" 2>/dev/null && ! pgrep -f "${TMP}/bin/keel-dsh-ref" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
kill -0 "${CHILD_PID}" 2>/dev/null && fail "sidecar grandchild ${CHILD_PID} survived shutdown"
if pgrep -f "${TMP}/bin/keel-dsh-ref" >/dev/null 2>&1; then
  fail "a reference sidecar process survived shutdown"
fi
pass "runtime exited; grandchild and sidecar gone (0 residue)"

log "SMOKE PASS ($((STEP)) steps)"
