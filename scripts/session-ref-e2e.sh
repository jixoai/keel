#!/usr/bin/env bash
# Session reference protection E2E (change keel-agent-team-v1, P3).
#
# What it proves against a real `keel serve` + PostgreSQL:
#   1. a live session bound to a team blocks `keel team delete` and
#      `keel team archive` with 409 team_in_use, and the CLI renders the
#      referencing session (state/runStatus) from the same references envelope;
#   2. archiving the session unbinds it: archive then delete the team succeed;
#   3. a live session bound to an agent blocks `keel agent delete|archive` with
#      409 agent_in_use; archiving the session then archiving and deleting the
#      agent succeed;
#   4. concurrent /input commits on one session leave sessions.bound_* equal to
#      the newest committed user.message selection (the SQL projection is the
#      ledger's newest fact, not the last response to land).
#
# Isolation: a dedicated database (DROP/CREATE ... WITH (FORCE)), two free
# loopback ports in the project's dev range, a local recording stub provider.
# Every process is reaped and the database dropped on exit.
#
# Usage: scripts/session-ref-e2e.sh [--evidence DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${KEEL_SESSION_REF_EVIDENCE:-$ROOT/openspec/changes/keel-agent-team-v1/evidence/session-ref-2026-11-21}"
DB="keel_session_ref_e2e_$(date +%Y%m%d_%H%M%S)"
PORT_RANGE_START=13610
PORT_RANGE_END=13799

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[session-ref-e2e] %s\n' "$*"; }
die() { printf '[session-ref-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

port_free() {
  python3 - "$1" <<'PY'
import socket, sys
s = socket.socket()
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
}

pick_port() {
  local excluded="${1:-}" p
  for p in $(seq "$PORT_RANGE_START" "$PORT_RANGE_END"); do
    case " $excluded " in *" $p "*) continue ;; esac
    if port_free "$p"; then echo "$p"; return 0; fi
  done
  die "no free port in $PORT_RANGE_START-$PORT_RANGE_END"
}

SERVE_PORT="$(pick_port)"
STUB_PORT="$(pick_port "$SERVE_PORT")"

# --- credentials/env (never echoed) ---------------------------------------
ENV_FILE="${FDE2E_ENV_PERSONAL:-$ROOT/.env.local}"
if [[ ! -f "$ENV_FILE" ]]; then
  ENV_FILE="$ROOT/../../.env.personal"
fi
[[ -f "$ENV_FILE" ]] || die "no env file found (.env.local or the main repo's .env.personal)"

DSN=""
while IFS= read -r line; do
  key="${line%%=*}"; value="${line#*=}"
  value="${value%\"}"; value="${value#\"}"; value="${value%\'}"; value="${value#\'}"
  case "$key" in
    KEEL_TEST_DSN) [[ -z "$DSN" ]] && DSN="$value" ;;
    KEEL_DSN) [[ -z "$DSN" ]] && DSN="$value" ;;
    KEEL_SERVE_PASSWORD) export KEEL_SERVE_PASSWORD="$value" ;;
  esac
done < <(grep -E '^(KEEL_[A-Z0-9_]+)=|^KEEL_DSN=' "$ENV_FILE")
[[ -n "$DSN" ]] || die "KEEL_TEST_DSN / KEEL_DSN is not set in $ENV_FILE"

TEST_DSN="$(KEEL_DSN_VALUE="$DSN" DB_NAME="$DB" python3 - <<'PY'
import os, re
dsn = os.environ["KEEL_DSN_VALUE"]
name = os.environ["DB_NAME"]
print(re.sub(r"/[^/?]+(\?|$)", "/" + name + r"\1", dsn, count=1))
PY
)"
DSN_PORT="$(KEEL_DSN_VALUE="$DSN" python3 - <<'PY'
import os, re
dsn = os.environ["KEEL_DSN_VALUE"]
m = re.match(r"postgres(?:ql)?://([^@]*@)?([^/:?]+)(:[0-9]+)?", dsn)
print((m.group(3) or ":5432").lstrip(":") if m else "5432")
PY
)"
PG_CTR="$(docker ps --format '{{.Names}}\t{{.Ports}}' | awk -F'\t' -v port=":$DSN_PORT->" 'index($2, port) { print $1; exit }')"
[[ -n "$PG_CTR" ]] || die "no postgres container publishes port $DSN_PORT"
log "postgres container: $PG_CTR (port $DSN_PORT)"

mkdir -p "$EVIDENCE"
log "evidence: $EVIDENCE"

CLEANUP_DONE=0
cleanup() {
  [[ "$CLEANUP_DONE" == 1 ]] && return 0
  CLEANUP_DONE=1
  [[ -n "${SERVE_PID:-}" ]] && kill "$SERVE_PID" 2>/dev/null || true
  [[ -n "${STUB_PID:-}" ]] && kill "$STUB_PID" 2>/dev/null || true
  [[ -n "${SERVE_PID:-}" ]] && wait "$SERVE_PID" 2>/dev/null || true
  [[ -n "${STUB_PID:-}" ]] && wait "$STUB_PID" 2>/dev/null || true
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1 || true
  rm -f "$EVIDENCE/cookies.txt"
  sleep 0.5
  {
    echo "# port release check"
    for p in "$SERVE_PORT" "$STUB_PORT"; do
      if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "port $p STILL LISTENING"
      else
        echo "port $p released (no listener)"
      fi
    done
  } > "$EVIDENCE/port-release.txt" 2>&1 || true
}
trap cleanup EXIT INT TERM

log "creating database $DB"
docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null
docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE \"$DB\"" >/dev/null

# --- recording stub provider ----------------------------------------------
cat > "$EVIDENCE/stub-provider.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
REPLY = "SESSION-REF-STUB-REPLY"

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        chunks = [
            {"id": "chatcmpl-session-ref", "object": "chat.completion.chunk",
             "created": 1735689600, "model": doc.get("model") or "stub",
             "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
            {"id": "chatcmpl-session-ref", "object": "chat.completion.chunk",
             "created": 1735689600, "model": doc.get("model") or "stub",
             "choices": [{"index": 0, "delta": {"content": REPLY}, "finish_reason": "stop"}],
             "usage": {"prompt_tokens": 0, "completion_tokens": 1, "total_tokens": 1}},
        ]
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks) + "data: [DONE]\n\n"
        encoded = body.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, *args):
        pass

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PY

python3 "$EVIDENCE/stub-provider.py" "$STUB_PORT" &
STUB_PID=$!
for _ in $(seq 1 60); do port_free "$STUB_PORT" || break; sleep 0.1; done
port_free "$STUB_PORT" && die "the stub provider did not start"

cat > "$EVIDENCE/models-config.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "session-ref",
      "name": "Session Ref E2E (recording stub)",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "SESSION_REF_KEY"},
      "modelIds": ["session-ref-model"],
      "visibility": "visible"
    }
  ],
  "selection": {"providerId": "session-ref", "modelId": "session-ref-model"}
}
JSON

export SESSION_REF_KEY="session-ref-not-a-credential"
KEEL_BIN="${KEEL_BIN:-/tmp/keel-session-ref-e2e}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

TENANT="$(uuidgen | tr '[:upper:]' '[:lower:]')"
KEEL_DSN="$TEST_DSN" KEEL_MODELS_CONFIG="$EVIDENCE/models-config.json" \
  "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$TENANT" \
  > "$EVIDENCE/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 100); do
  if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
  kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $EVIDENCE/serve.log)"
  sleep 0.1
done
curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

# The placement gate's groups row carries a tenant FK, so provision the tenant
# row the serve process acts as before any input is admitted.
docker exec "$PG_CTR" psql -U postgres -d "$DB" -c \
  "INSERT INTO tenants (tenant_id, name) VALUES ('$TENANT', 'session-ref-e2e') ON CONFLICT (tenant_id) DO NOTHING" \
  >/dev/null

export KEEL_SERVE_ADDR="127.0.0.1:$SERVE_PORT"
BASE="http://127.0.0.1:$SERVE_PORT"
JAR="$EVIDENCE/cookies.txt"
curl -sS -c "$JAR" -X POST "$BASE/v1/auth/login" -H 'Content-Type: application/json' \
  --data-binary "$(printf '{"password":"%s"}' "${KEEL_SERVE_PASSWORD:-}")" > "$EVIDENCE/login.json"
grep -q '"authenticated":true' "$EVIDENCE/login.json" || die "login failed: $(cat "$EVIDENCE/login.json")"

api() { # METHOD PATH [BODY] -> body stdout, status last-status
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -b "$JAR" -X "$method" "$BASE$path" -H 'Content-Type: application/json' \
      --data-binary "$body" -o "$EVIDENCE/last-body.json" -w '%{http_code}'
  else
    curl -sS -b "$JAR" -X "$method" "$BASE$path" -o "$EVIDENCE/last-body.json" -w '%{http_code}'
  fi
}
json_get() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2],""))' "$EVIDENCE/last-body.json" "$1"; }
wait_run() { # SESSION WANT
  local session="$1" want="$2" status=""
  for _ in $(seq 1 250); do
    api GET "/v1/sessions/$session/run" >/dev/null
    status="$(json_get status)"
    case "$status" in done|failed|cancelled) break ;; esac
    sleep 0.2
  done
  [[ "$status" == "$want" ]] || die "session $session settled as '${status:-unknown}', want $want"
}

psql_q() { docker exec "$PG_CTR" psql -U postgres -d "$DB" -At -c "$1"; }

# --- 1. team: live session blocks delete + archive -------------------------
log "team: live session blocks delete/archive"
"$KEEL_BIN" agent create --id agent_ref_captain --name "Ref 队长" \
  --provider session-ref --model session-ref-model > "$EVIDENCE/team-captain.txt" 2>&1
"$KEEL_BIN" team create --id team_ref --name "Ref 小队" --captain agent_ref_captain \
  > "$EVIDENCE/team-created.txt" 2>&1
"$KEEL_BIN" team start team_ref --text "team session ref" > "$EVIDENCE/team-start.txt" 2>&1
TEAM_SESSION="$(sed -n 's/^  sessionId: //p' "$EVIDENCE/team-start.txt")"
[[ -n "$TEAM_SESSION" ]] || die "team start did not print a sessionId: $(cat "$EVIDENCE/team-start.txt")"
wait_run "$TEAM_SESSION" done

set +e
"$KEEL_BIN" team delete team_ref > "$EVIDENCE/team-delete-blocked.txt" 2>&1
CODE=$?
set -e
[[ "$CODE" -ne 0 ]] || die "team delete with a bound session exited 0"
grep -q "409 team_in_use" "$EVIDENCE/team-delete-blocked.txt" || die "missing 409 team_in_use"
grep -q "referencing session: $TEAM_SESSION" "$EVIDENCE/team-delete-blocked.txt" || \
  die "CLI did not render the referencing session: $(cat "$EVIDENCE/team-delete-blocked.txt")"

set +e
"$KEEL_BIN" team archive team_ref > "$EVIDENCE/team-archive-blocked.txt" 2>&1
CODE=$?
set -e
[[ "$CODE" -ne 0 ]] || die "team archive with a bound session exited 0"
grep -q "409 team_in_use" "$EVIDENCE/team-archive-blocked.txt" || die "missing archive 409 team_in_use"

# Archive the session (soft delete) = unbind; both archive and delete succeed.
STATUS="$(api DELETE "/v1/sessions/$TEAM_SESSION")"
[[ "$STATUS" == "200" ]] || die "archive session: status $STATUS"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/session-archive-team.json"
"$KEEL_BIN" team archive team_ref > "$EVIDENCE/team-archive.txt" 2>&1
grep -q "archived team team_ref" "$EVIDENCE/team-archive.txt" || die "team archive failed: $(cat "$EVIDENCE/team-archive.txt")"
"$KEEL_BIN" team delete team_ref > "$EVIDENCE/team-delete-archived.txt" 2>&1
grep -q "deleted team team_ref" "$EVIDENCE/team-delete-archived.txt" || die "archived team delete failed"

# --- 2. agent: live session blocks delete + archive -----------------------
log "agent: live session blocks delete/archive"
"$KEEL_BIN" agent create --id agent_ref_solo --name "Ref Agent" \
  --provider session-ref --model session-ref-model > "$EVIDENCE/agent-created.txt" 2>&1
"$KEEL_BIN" agent start agent_ref_solo --text "agent session ref" > "$EVIDENCE/agent-start.txt" 2>&1
AGENT_SESSION="$(sed -n 's/^  sessionId: //p' "$EVIDENCE/agent-start.txt")"
[[ -n "$AGENT_SESSION" ]] || die "agent start did not print a sessionId"
wait_run "$AGENT_SESSION" done

set +e
"$KEEL_BIN" agent delete agent_ref_solo > "$EVIDENCE/agent-delete-blocked.txt" 2>&1
CODE=$?
set -e
[[ "$CODE" -ne 0 ]] || die "agent delete with a bound session exited 0"
grep -q "409 agent_in_use" "$EVIDENCE/agent-delete-blocked.txt" || die "missing 409 agent_in_use"
grep -q "referencing session: $AGENT_SESSION" "$EVIDENCE/agent-delete-blocked.txt" || \
  die "CLI did not render the referencing session: $(cat "$EVIDENCE/agent-delete-blocked.txt")"

set +e
"$KEEL_BIN" agent archive agent_ref_solo > "$EVIDENCE/agent-archive-blocked.txt" 2>&1
CODE=$?
set -e
[[ "$CODE" -ne 0 ]] || die "agent archive with a bound session exited 0"
grep -q "409 agent_in_use" "$EVIDENCE/agent-archive-blocked.txt" || die "missing archive 409 agent_in_use"

STATUS="$(api DELETE "/v1/sessions/$AGENT_SESSION")"
[[ "$STATUS" == "200" ]] || die "archive agent session: status $STATUS"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/session-archive-agent.json"
"$KEEL_BIN" agent archive agent_ref_solo > "$EVIDENCE/agent-archive.txt" 2>&1
grep -q "archived agent agent_ref_solo" "$EVIDENCE/agent-archive.txt" || die "agent archive failed: $(cat "$EVIDENCE/agent-archive.txt")"
"$KEEL_BIN" agent delete agent_ref_solo > "$EVIDENCE/agent-delete.txt" 2>&1
grep -q "deleted agent agent_ref_solo" "$EVIDENCE/agent-delete.txt" || die "archived agent delete failed"

# --- 3. concurrent inputs: newest seq owns the projection -----------------
log "concurrency: newest committed selection owns the projection"
"$KEEL_BIN" agent create --id agent_conc_a --name "Conc A" \
  --provider session-ref --model session-ref-model > "$EVIDENCE/conc-a.txt" 2>&1
"$KEEL_BIN" agent create --id agent_conc_b --name "Conc B" \
  --provider session-ref --model session-ref-model > "$EVIDENCE/conc-b.txt" 2>&1
CONC_SESSION="$(uuidgen | tr '[:upper:]' '[:lower:]')"
STATUS="$(api POST /v1/sessions "{\"sessionId\":\"$CONC_SESSION\"}")"
[[ "$STATUS" == "200" || "$STATUS" == "201" ]] || die "create concession: status $STATUS"

PIDS=()
for i in $(seq 0 7); do
  agent_id="agent_conc_a"
  [[ $((i % 2)) -eq 1 ]] && agent_id="agent_conc_b"
  curl -sS -b "$JAR" -X POST "$BASE/v1/sessions/$CONC_SESSION/input" \
    -H 'Content-Type: application/json' \
    --data-binary "{\"text\":\"conc $i\",\"requestId\":\"conc-$i\",\"selection\":{\"agentId\":\"$agent_id\"}}" \
    -o /dev/null &
  PIDS+=("$!")
done
for pid in "${PIDS[@]}"; do wait "$pid"; done

BOUND="$(psql_q "SELECT CONCAT(bound_agent_id, '|', bound_seq) FROM sessions WHERE tenant_id = '$TENANT' AND session_id = '$CONC_SESSION'")"
NEWEST="$(psql_q "SELECT CONCAT(payload->'selection'->>'agentId', '|', seq) FROM events WHERE tenant_id = '$TENANT' AND session_id = '$CONC_SESSION' AND kind = 'user.message' AND payload ? 'selection' ORDER BY seq DESC LIMIT 1")"
[[ "$BOUND" == "$NEWEST" ]] || die "projection ${BOUND} != newest ledger selection ${NEWEST}"
{
  echo "session: $CONC_SESSION"
  echo "sessions.bound_agent_id|bound_seq: $BOUND"
  echo "newest user.message selection|seq: $NEWEST"
  echo "writers: 8 (alternating agent_conc_a/agent_conc_b)"
} > "$EVIDENCE/concurrency.txt"
log "projection matches newest ledger selection: $BOUND"

# --- evidence summary ------------------------------------------------------
python3 - "$EVIDENCE" "$TENANT" "$TEAM_SESSION" "$AGENT_SESSION" "$CONC_SESSION" "$BOUND" <<'PY'
import json, sys
root, tenant, team_session, agent_session, conc_session, bound = sys.argv[1:]
summary = {
  "tenant": tenant,
  "teamSession": team_session,
  "agentSession": agent_session,
  "concurrencySession": conc_session,
  "projection": bound,
  "checks": {
    "teamDeleteBlocked409": True,
    "teamArchiveBlocked409": True,
    "teamArchiveAfterSessionArchive": True,
    "teamDeleteArchived": True,
    "agentDeleteBlocked409": True,
    "agentArchiveBlocked409": True,
    "agentArchiveAfterSessionArchive": True,
    "agentDeleteArchived": True,
    "concurrentProjectionMatchesLedger": True,
  },
}
json.dump(summary, open(root + "/checks.json", "w"), ensure_ascii=False, indent=2)
print("checks:", json.dumps(summary["checks"], ensure_ascii=False))
PY

log "PASS — evidence in $EVIDENCE"
