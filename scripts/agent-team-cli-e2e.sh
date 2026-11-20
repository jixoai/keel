#!/usr/bin/env bash
# CLI tail E2E (change keel-agent-team-v1, tasks P2/P3): `keel agent start|stop`
# and `keel team save|start` against a real `keel serve` and a dedicated
# PostgreSQL database.
#
# What it proves:
#   1. agent start creates a session and queues a run that settles `done`; the
#      run.finished freezes the agentId of the selected Agent.
#   2. agent stop on an idle Agent exits nonzero with an explicit "no active
#      run" (never a fake cancellation); on a live run it cancels through
#      POST /v1/sessions/{id}/cancel and the run.finished is `cancelled`.
#   3. team save upserts a YAML team (create then update, revision moves on the
#      server); a `team show` JSON export round-trips through save (server-owned
#      fields stripped, revision 3); an id-less document is refused locally.
#   4. team start queues a run whose selection is teamId; the captain answers and
#      run.finished freezes teamId/agentId/teamRevision.
#   5. agent stop matches a team session through its captain and cancels it.
#   6. deleting a member Agent a team seats is refused with 409 agent_in_use and
#      the CLI renders the referencing team.
#
# Isolation: a dedicated database (DROP/CREATE ... WITH (FORCE)), two free
# loopback ports inside the project's dev range, a local recording stub provider
# (no upstream credential). Every process is reaped and the database dropped on
# exit.
#
# Usage: scripts/agent-team-cli-e2e.sh [--evidence DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${KEEL_CLI_E2E_EVIDENCE:-$ROOT/openspec/changes/keel-agent-team-v1/evidence/cli-tail-2026-11-20}"
DB="keel_agent_team_cli_e2e_$(date +%Y%m%d_%H%M%S)"
PORT_RANGE_START=13610
PORT_RANGE_END=13799

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[cli-tail-e2e] %s\n' "$*"; }
die() { printf '[cli-tail-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

# port_free PORT: succeeds when 127.0.0.1:PORT can be bound.
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

# pick_port EXCLUDED: first free port in the dev range, never the caller's own
# earlier pick (a command substitution runs in a subshell, so the choice travels
# back through the argument).
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

# --- recording stub provider (slow when the user text says SLOW) -----------
cat > "$EVIDENCE/stub-provider.py" <<'PY'
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
BODIES = sys.argv[2]
REPLY = "KITE-CLI-STUB-REPLY 已收到。"

def record(doc):
    with open(BODIES, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

def chunks(model):
    def wrap(delta, finish=None, usage=None):
        out = {
            "id": "chatcmpl-kite-cli-stub", "object": "chat.completion.chunk",
            "created": 1735689600, "model": model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        }
        if usage is not None:
            out["usage"] = usage
        return out
    yield wrap({"role": "assistant"})
    for start in range(0, len(REPLY), 6):
        yield wrap({"content": REPLY[start:start + 6]})
    yield wrap({}, finish="stop",
               usage={"prompt_tokens": 0, "completion_tokens": len(REPLY), "total_tokens": len(REPLY)})

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        text = " ".join(
            str(m.get("content") or "") for m in (doc.get("messages") or []) if m.get("role") == "user"
        )
        record({
            "path": self.path,
            "model": doc.get("model"),
            "messages": doc.get("messages"),
            "tools": [t.get("function", {}).get("name") for t in (doc.get("tools") or [])],
            "slow": "SLOW" in text,
        })
        if "SLOW" in text:
            # Hold the turn open so the E2E can stop a genuinely running run.
            time.sleep(8)
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks(doc.get("model") or "stub"))
        body += "data: [DONE]\n\n"
        encoded = body.encode("utf-8")
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(encoded)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(encoded)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PY

: > "$EVIDENCE/wire-requests.jsonl"
python3 "$EVIDENCE/stub-provider.py" "$STUB_PORT" "$EVIDENCE/wire-requests.jsonl" &
STUB_PID=$!
for _ in $(seq 1 60); do port_free "$STUB_PORT" || break; sleep 0.1; done
port_free "$STUB_PORT" && die "the stub provider did not start"

cat > "$EVIDENCE/models-config.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "kite-cli",
      "name": "Kite CLI E2E (recording stub)",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_CLI_KEY"},
      "modelIds": ["kite-cli-model"],
      "visibility": "visible"
    }
  ],
  "selection": {"providerId": "kite-cli", "modelId": "kite-cli-model"}
}
JSON

export KITE_CLI_KEY="kite-cli-not-a-credential"
KEEL_BIN="${KEEL_BIN:-/tmp/keel-agent-team-cli-e2e}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

KEEL_DSN="$TEST_DSN" KEEL_MODELS_CONFIG="$EVIDENCE/models-config.json" \
  "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$(uuidgen | tr '[:upper:]' '[:lower:]')" \
  --web-dir "$ROOT/web/dist" > "$EVIDENCE/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 100); do
  if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
  kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $EVIDENCE/serve.log)"
  sleep 0.1
done
curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

export KEEL_SERVE_ADDR="127.0.0.1:$SERVE_PORT"
BASE="http://127.0.0.1:$SERVE_PORT"
JAR="$EVIDENCE/cookies.txt"
curl -sS -c "$JAR" -X POST "$BASE/v1/auth/login" -H 'Content-Type: application/json' \
  --data-binary "$(printf '{"password":"%s"}' "${KEEL_SERVE_PASSWORD:-}")" > "$EVIDENCE/login.json"
grep -q '"authenticated":true' "$EVIDENCE/login.json" || die "login failed: $(cat "$EVIDENCE/login.json")"

api() { # METHOD PATH [BODY] -> body on stdout, status in last-status
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
    case "$status" in
      done|failed|cancelled) break ;;
    esac
    sleep 0.2
  done
  case "$status" in
    "$want") return 0 ;;
    *) die "session $session settled as '${status:-unknown}', want $want" ;;
  esac
}

# --- 1. agent start -> run done -------------------------------------------
log "creating the probe agent and starting a run"
"$KEEL_BIN" agent create --id agent_cli_probe --name "CLI Probe" \
  --provider kite-cli --model kite-cli-model \
  --prompt "KITE-CLI-MARK-1 cli probe instructions" > "$EVIDENCE/agent-created.txt" 2>&1
"$KEEL_BIN" agent start agent_cli_probe --text "probe: say hello" > "$EVIDENCE/agent-start.txt" 2>&1
SESSION="$(sed -n 's/^  sessionId: //p' "$EVIDENCE/agent-start.txt")"
RUN_ID="$(sed -n 's/^  runId: //p' "$EVIDENCE/agent-start.txt")"
[[ -n "$SESSION" && -n "$RUN_ID" ]] || die "agent start did not print sessionId/runId: $(cat "$EVIDENCE/agent-start.txt")"
wait_run "$SESSION" done
api GET "/v1/sessions/$SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/agent-events.json"
python3 - "$EVIDENCE" "$SESSION" <<'PY'
import json, sys
ev = json.load(open(sys.argv[1] + "/agent-events.json"))["events"]
user = [e for e in ev if e["kind"] == "user.message"][0]["payload"]
assert user["selection"]["agentId"] == "agent_cli_probe", user
summary = [e for e in ev if e["kind"] == "run.finished"][-1]["payload"]
assert summary["status"] == "done", summary
assert summary.get("agentId") == "agent_cli_probe", summary
print("agent run settled:", summary.get("status"), summary.get("agentId"))
PY

# --- 2. stop with nothing active is a refusal ------------------------------
log "stopping an idle agent (must refuse, not fake a cancellation)"
set +e
"$KEEL_BIN" agent stop agent_cli_probe > "$EVIDENCE/agent-stop-idle.txt" 2>&1
IDLE_CODE=$?
set -e
[[ "$IDLE_CODE" -ne 0 ]] || die "agent stop on an idle agent exited 0"
grep -q "no active run for agent agent_cli_probe" "$EVIDENCE/agent-stop-idle.txt" || \
  die "idle stop did not name the no-active state: $(cat "$EVIDENCE/agent-stop-idle.txt")"

# --- 3. stop a genuinely running run ---------------------------------------
log "starting a slow run and stopping it"
"$KEEL_BIN" agent start agent_cli_probe --text "SLOW: hold this turn" > "$EVIDENCE/agent-slow-start.txt" 2>&1
SLOW_SESSION="$(sed -n 's/^  sessionId: //p' "$EVIDENCE/agent-slow-start.txt")"
[[ -n "$SLOW_SESSION" ]] || die "slow start did not print a sessionId"
for _ in $(seq 1 100); do
  api GET "/v1/sessions/$SLOW_SESSION/run" >/dev/null
  [[ "$(json_get status)" == "running" ]] && break
  sleep 0.1
done
[[ "$(json_get status)" == "running" ]] || die "the slow run never reached running"
"$KEEL_BIN" agent stop agent_cli_probe > "$EVIDENCE/agent-stop-live.txt" 2>&1
grep -q "stopped session $SLOW_SESSION" "$EVIDENCE/agent-stop-live.txt" || \
  die "live stop did not report a cancellation: $(cat "$EVIDENCE/agent-stop-live.txt")"
wait_run "$SLOW_SESSION" failed
api GET "/v1/sessions/$SLOW_SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/agent-cancelled-events.json"
python3 - "$EVIDENCE" <<'PY'
import json, sys
ev = json.load(open(sys.argv[1] + "/agent-cancelled-events.json"))["events"]
summary = [e for e in ev if e["kind"] == "run.finished"][-1]["payload"]
assert summary["status"] == "cancelled", summary
print("cancelled run.finished:", summary.get("status"))
PY

# --- 4. team save (create then update) -------------------------------------
log "saving a team document twice (create then update)"
"$KEEL_BIN" agent create --id agent_cli_member --name "CLI Member" \
  --provider kite-cli --model kite-cli-model > "$EVIDENCE/member-created.txt" 2>&1
cat > "$EVIDENCE/team.yaml" <<YAML
id: team_cli_probe
name: CLI 小队
description: first save
captain:
  agentId: agent_cli_probe
members:
  - agentId: agent_cli_member
    label: 评审
defaults:
  tools: []
capabilities:
  teamCli:
    enabled: true
    commands: [member.add]
YAML
"$KEEL_BIN" team save "$EVIDENCE/team.yaml" > "$EVIDENCE/team-save-1.txt" 2>&1
grep -q "created revision=1" "$EVIDENCE/team-save-1.txt" || die "first team save: $(cat "$EVIDENCE/team-save-1.txt")"
sed -i.bak 's/^description: first save$/description: second save/' "$EVIDENCE/team.yaml"
rm -f "$EVIDENCE/team.yaml.bak"
"$KEEL_BIN" team save "$EVIDENCE/team.yaml" > "$EVIDENCE/team-save-2.txt" 2>&1
grep -q "updated revision=2" "$EVIDENCE/team-save-2.txt" || die "second team save: $(cat "$EVIDENCE/team-save-2.txt")"
"$KEEL_BIN" team show team_cli_probe > "$EVIDENCE/team-show.json" 2>&1
python3 - "$EVIDENCE" <<'PY'
import json, sys
team = json.load(open(sys.argv[1] + "/team-show.json"))
assert team["revision"] == 2, team
assert team["description"] == "second save", team
print("team revision after upsert:", team["revision"])
PY

# The show export carries tenantId/status/revision/hash; saving it back must
# strip them and move the revision, not replay the exported one.
"$KEEL_BIN" team show team_cli_probe > "$EVIDENCE/team-show-export.json" 2>&1
python3 - "$EVIDENCE" <<'PY'
import json, sys
team = json.load(open(sys.argv[1] + "/team-show-export.json"))
for key in ("tenantId", "status", "revision", "hash"):
    assert key in team, (key, team)
print("show export carries server-owned fields:", sorted(team))
PY
"$KEEL_BIN" team save "$EVIDENCE/team-show-export.json" > "$EVIDENCE/team-save-3.txt" 2>&1
grep -q "updated revision=3" "$EVIDENCE/team-save-3.txt" || die "show export round-trip: $(cat "$EVIDENCE/team-save-3.txt")"

# save is an upsert: without a stable id the CLI refuses locally (nothing is
# created, so a repeated save cannot duplicate a team).
cat > "$EVIDENCE/team-no-id.yaml" <<YAML
name: 无 id 队
captain:
  agentId: agent_cli_probe
YAML
set +e
"$KEEL_BIN" team save "$EVIDENCE/team-no-id.yaml" > "$EVIDENCE/team-save-no-id.txt" 2>&1
NO_ID_CODE=$?
set -e
[[ "$NO_ID_CODE" -eq 2 ]] || die "an id-less save exited $NO_ID_CODE, want 2: $(cat "$EVIDENCE/team-save-no-id.txt")"
grep -q "must carry id" "$EVIDENCE/team-save-no-id.txt" || \
  die "the id-less refusal is not explicit: $(cat "$EVIDENCE/team-save-no-id.txt")"

# --- 5. team start ---------------------------------------------------------
log "starting the team through the CLI"
"$KEEL_BIN" team start team_cli_probe --text "team probe" > "$EVIDENCE/team-start.txt" 2>&1
TEAM_SESSION="$(sed -n 's/^  sessionId: //p' "$EVIDENCE/team-start.txt")"
[[ -n "$TEAM_SESSION" ]] || die "team start did not print a sessionId: $(cat "$EVIDENCE/team-start.txt")"
wait_run "$TEAM_SESSION" done
api GET "/v1/sessions/$TEAM_SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/team-events.json"
python3 - "$EVIDENCE" <<'PY'
import json, sys
ev = json.load(open(sys.argv[1] + "/team-events.json"))["events"]
user = [e for e in ev if e["kind"] == "user.message"][0]["payload"]
assert user["selection"]["teamId"] == "team_cli_probe", user
summary = [e for e in ev if e["kind"] == "run.finished"][-1]["payload"]
assert summary["status"] == "done", summary
assert summary.get("teamId") == "team_cli_probe", summary
assert summary.get("agentId") == "agent_cli_probe", summary
assert summary.get("teamRevision") == 3, summary
print("team run.finished:", summary.get("teamId"), summary.get("agentId"), summary.get("teamRevision"))
PY

# --- 5b. stop a team run through its captain -------------------------------
log "starting a slow team run and stopping it through the captain"
"$KEEL_BIN" team start team_cli_probe --text "SLOW: hold the team turn" > "$EVIDENCE/team-slow-start.txt" 2>&1
SLOW_TEAM_SESSION="$(sed -n 's/^  sessionId: //p' "$EVIDENCE/team-slow-start.txt")"
[[ -n "$SLOW_TEAM_SESSION" ]] || die "slow team start did not print a sessionId"
for _ in $(seq 1 100); do
  api GET "/v1/sessions/$SLOW_TEAM_SESSION/run" >/dev/null
  [[ "$(json_get status)" == "running" ]] && break
  sleep 0.1
done
[[ "$(json_get status)" == "running" ]] || die "the slow team run never reached running"
"$KEEL_BIN" agent stop agent_cli_probe > "$EVIDENCE/team-stop-live.txt" 2>&1
grep -q "stopped session $SLOW_TEAM_SESSION" "$EVIDENCE/team-stop-live.txt" || \
  die "stopping a team run through its captain failed: $(cat "$EVIDENCE/team-stop-live.txt")"
wait_run "$SLOW_TEAM_SESSION" failed
api GET "/v1/sessions/$SLOW_TEAM_SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/team-cancelled-events.json"
python3 - "$EVIDENCE" <<'PY'
import json, sys
ev = json.load(open(sys.argv[1] + "/team-cancelled-events.json"))["events"]
summary = [e for e in ev if e["kind"] == "run.finished"][-1]["payload"]
assert summary["status"] == "cancelled", summary
assert summary.get("teamId") == "team_cli_probe", summary
print("cancelled team run.finished:", summary.get("status"), summary.get("teamId"))
PY

# --- 6. reference protection ----------------------------------------------
log "deleting a seated member agent (must be refused with 409)"
set +e
"$KEEL_BIN" agent delete agent_cli_member > "$EVIDENCE/agent-delete-blocked.txt" 2>&1
DELETE_CODE=$?
set -e
[[ "$DELETE_CODE" -ne 0 ]] || die "deleting a seated member agent exited 0"
grep -q "409 agent_in_use" "$EVIDENCE/agent-delete-blocked.txt" || \
  die "the delete refusal is not 409 agent_in_use: $(cat "$EVIDENCE/agent-delete-blocked.txt")"
grep -q "referencing team: team_cli_probe CLI 小队" "$EVIDENCE/agent-delete-blocked.txt" || \
  die "the delete refusal does not list the team: $(cat "$EVIDENCE/agent-delete-blocked.txt")"

# --- evidence summary ------------------------------------------------------
python3 - "$EVIDENCE" <<'PY'
import json, sys
root = sys.argv[1]
rows = [json.loads(line) for line in open(root + "/wire-requests.jsonl") if line.strip()]
assert rows, "no provider request was recorded"
def system_of(row):
    for m in row.get("messages") or []:
        if m.get("role") == "system":
            return m.get("content") or ""
    return ""
assert any("KITE-CLI-MARK-1" in system_of(r) for r in rows), "the agent prompt never reached the wire"
summary = {
    "providerRequests": len(rows),
    "slowRequests": sum(1 for r in rows if r.get("slow")),
    "systemPromptCarried": True,
}
json.dump(summary, open(root + "/wire-assertions.json", "w"), ensure_ascii=False, indent=2)
print("wire assertions:", summary)
PY

log "PASS — evidence in $EVIDENCE"
