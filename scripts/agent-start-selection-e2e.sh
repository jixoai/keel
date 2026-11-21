#!/usr/bin/env bash
# Startup-selection E2E for P7 (change keel-agent-team-v1).
#
# What it proves, against a real PostgreSQL database and a real `keel serve`
# whose provider is a local recording stub:
#
#   Leg A — Standard start: an input whose selection names a model starts the
#     run on that model. `run.finished` freezes the resolved provider/model
#     (and records no agent/team binding).
#
#   Leg B — Team start: an input whose selection names only a team starts the
#     run as that team's captain. `run.finished` freezes the captain agent id,
#     team id, the stored team revision and content hash, and a prompt hash;
#     the captain's default model answers even though the selection named no
#     model.
#
#   Leg C — Switching while a run is live only affects the next run:
#     run 1 starts standard (slow model); while its provider request is
#     in flight, a team input is queued; run 2 starts team (slow model); while
#     that request is in flight, a standard input is queued; run 3 starts
#     standard on its own model. Each run.finished carries the identity of the
#     input that started that run, never the binding chosen mid-flight.
#
# The stub provider deliberately delays the slow models (`kite-slow`,
# `kite-team-slow`) before answering,
# so the script can queue the next input after the attempt's provider request
# has been recorded — the attempt's identity is already frozen at that point.
#
# Isolation: a dedicated database (DROP/CREATE ... WITH (FORCE)) and free
# loopback ports inside the project's dev range (13610-13799). The session
# cookie is deleted before exit; the DSN and passphrase are never printed.
#
# Usage: scripts/agent-start-selection-e2e.sh [--evidence DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
EVIDENCE="${KEEL_AGENT_START_E2E_EVIDENCE:-$ROOT/openspec/changes/keel-agent-team-v1/evidence/start-selection-$STAMP}"
DB="keel_agent_start_e2e_$STAMP"
DB="${DB//-/_}"
TENANT="$(uuidgen | tr '[:upper:]' '[:lower:]')"
PORT_RANGE_START=13610
PORT_RANGE_END=13799
SLOW_SECONDS="${KEEL_AGENT_START_E2E_SLOW_SECONDS:-2.5}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[agent-start-e2e] %s\n' "$*"; }
die() { printf '[agent-start-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

# --- credentials/env -------------------------------------------------------
ENV_FILE="${FDE2E_ENV_PERSONAL:-$ROOT/.env.local}"
if [[ ! -f "$ENV_FILE" ]]; then
  # <main>/.worktrees/<name>/scripts/../.. = <main>
  ENV_FILE="$ROOT/../../.env.personal"
fi
[[ -f "$ENV_FILE" ]] || die "no env file found (.env.local or the main repo's .env.personal)"

# Only KEY-shaped assignments are read; nothing is echoed.
DSN=""
while IFS= read -r line; do
  key="${line%%=*}"; value="${line#*=}"
  value="${value%\"}"; value="${value#\"}"; value="${value%\'}"; value="${value#\'}"
  case "$key" in
    KEEL_DSN) DSN="$value" ;;
    KEEL_SERVE_PASSWORD) export KEEL_SERVE_PASSWORD="$value" ;;
  esac
done < <(grep -E '^(KEEL_[A-Z0-9_]+)=|^KEEL_DSN=' "$ENV_FILE")
[[ -n "$DSN" ]] || die "KEEL_DSN is not set in $ENV_FILE"
[[ -n "${KEEL_SERVE_PASSWORD:-}" ]] || die "KEEL_SERVE_PASSWORD is not set in $ENV_FILE"

# The test DSN: same server, a dedicated database name.
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
match = re.match(r"postgres(?:ql)?://([^@]*@)?([^/:?]+)(:[0-9]+)?", dsn)
print((match.group(3) or ":5432").lstrip(":") if match else "5432")
PY
)"
PG_CTR="$(docker ps --format '{{.Names}}\t{{.Ports}}' | awk -F'\t' -v port=":$DSN_PORT->" 'index($2, port) { print $1; exit }')"
[[ -n "$PG_CTR" ]] || die "no postgres container publishes port $DSN_PORT"
log "postgres container: $PG_CTR (port $DSN_PORT)"

# --- ports -----------------------------------------------------------------
port_free() { # PORT
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
  # The session cookie is a credential-shaped artifact: never kept in evidence.
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

# --- database --------------------------------------------------------------
log "creating database $DB"
docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null
docker exec "$PG_CTR" psql -U postgres -c "CREATE DATABASE \"$DB\"" >/dev/null

# --- recording, delaying stub provider ------------------------------------
cat > "$EVIDENCE/stub-provider.py" <<'PY'
# The recording stub: captures every /v1/chat/completions request body and
# answers with a valid OpenAI streaming response. The slow models are
# delayed before the response starts, giving the driver a window in which the
# attempt is demonstrably in flight (the request is already recorded).
import json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
BODIES = sys.argv[2]
LOG = sys.argv[3]
SLOW_SECONDS = float(sys.argv[4])

REPLY = "KITE-START-STUB-REPLY 已收到。"

def record(doc):
    with open(BODIES, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

def chunks(model):
    def wrap(delta, finish=None, usage=None):
        out = {
            "id": "chatcmpl-kite-start-stub",
            "object": "chat.completion.chunk",
            "created": 1735689600,
            "model": model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        }
        if usage is not None:
            out["usage"] = usage
        return out
    yield wrap({"role": "assistant"})
    for start in range(0, len(REPLY), 6):
        yield wrap({"content": REPLY[start:start + 6]})
    yield wrap({}, finish="stop", usage={"prompt_tokens": 0, "completion_tokens": len(REPLY), "total_tokens": len(REPLY)})

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        auth = self.headers.get("Authorization") or ""
        model = doc.get("model") or "stub"
        tools = [t.get("function", {}).get("name") for t in (doc.get("tools") or [])]
        record({
            "path": self.path,
            "model": model,
            "stream": doc.get("stream"),
            "messages": doc.get("messages"),
            "tools": tools,
            "authPresent": bool(auth),
        })
        with open(LOG, "a", encoding="utf-8") as fh:
            fh.write("POST %s model=%s tools=%s\n" % (self.path, model, tools))
        if model in ("kite-slow", "kite-team-slow"):
            time.sleep(SLOW_SECONDS)
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks(model))
        body += "data: [DONE]\n\n"
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

: > "$EVIDENCE/wire-requests.jsonl"
: > "$EVIDENCE/stub-provider.log"
python3 "$EVIDENCE/stub-provider.py" "$STUB_PORT" "$EVIDENCE/wire-requests.jsonl" "$EVIDENCE/stub-provider.log" "$SLOW_SECONDS" &
STUB_PID=$!
for _ in $(seq 1 60); do port_free "$STUB_PORT" || break; sleep 0.1; done
port_free "$STUB_PORT" && die "the stub provider did not start"

# --- models document + serve ----------------------------------------------
cat > "$EVIDENCE/models-config.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "kite-e2e",
      "name": "Kite Start E2E (recording stub)",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_START_E2E_KEY"},
      "modelIds": ["kite-standard-a", "kite-standard-b", "kite-slow", "kite-team-slow"],
      "visibility": "visible"
    }
  ],
  "selection": {"providerId": "kite-e2e", "modelId": "kite-standard-a"}
}
JSON

export KITE_START_E2E_KEY="kite-start-e2e-not-a-credential"
KEEL_BIN="${KEEL_BIN:-/tmp/keel-agent-start-e2e}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

KEEL_DSN="$TEST_DSN" KEEL_TEST_DSN="$TEST_DSN" KEEL_MODELS_CONFIG="$EVIDENCE/models-config.json" \
  "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$TENANT" \
  > "$EVIDENCE/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 100); do
  if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
  kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $EVIDENCE/serve.log)"
  sleep 0.1
done
curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

# The product deployment provisions tenants out of band; the harness seeds the
# one row the placement gate's groups FK requires. This is fixture setup, not a
# product write.
docker exec "$PG_CTR" psql -U postgres -d "$DB" -c \
  "INSERT INTO tenants (tenant_id, name) VALUES ('$TENANT', 'p7 start-selection e2e') ON CONFLICT DO NOTHING" >/dev/null

BASE="http://127.0.0.1:$SERVE_PORT"
JAR="$EVIDENCE/cookies.txt"
curl -sS -c "$JAR" -X POST "$BASE/v1/auth/login" -H 'Content-Type: application/json' \
  --data-binary "$(printf '{"password":"%s"}' "$KEEL_SERVE_PASSWORD")" > "$EVIDENCE/login.json"
grep -q '"authenticated":true' "$EVIDENCE/login.json" || die "login failed: $(cat "$EVIDENCE/login.json")"

api() { # METHOD PATH [BODY]  → body on stdout, status in $EVIDENCE/last-status
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -b "$JAR" -X "$method" "$BASE$path" -H 'Content-Type: application/json' \
      --data-binary "$body" -o "$EVIDENCE/last-body.json" -w '%{http_code}'
  else
    curl -sS -b "$JAR" -X "$method" "$BASE$path" -o "$EVIDENCE/last-body.json" -w '%{http_code}'
  fi
}

new_session() { # ID
  local id="$1"
  code="$(api POST /v1/sessions "$(python3 -c 'import json,sys;print(json.dumps({"sessionId":sys.argv[1]}))' "$id")")"
  [[ "$code" == "201" ]] || die "create session $id: HTTP $code $(cat "$EVIDENCE/last-body.json")"
}

send_input() { # SESSION TEXT REQUEST_ID SELECTION_JSON
  local session="$1" text="$2" request="$3" selection="$4"
  local body
  body="$(SESSION="$session" TEXT="$text" REQUEST="$request" SELECTION="$selection" python3 - <<'PY'
import json, os
print(json.dumps({
    "text": os.environ["TEXT"],
    "requestId": os.environ["REQUEST"],
    "selection": json.loads(os.environ["SELECTION"]),
}))
PY
)"
  code="$(api POST "/v1/sessions/$session/input" "$body")"
  [[ "$code" == "201" ]] || die "input $request: HTTP $code $(cat "$EVIDENCE/last-body.json")"
  cp "$EVIDENCE/last-body.json" "$EVIDENCE/input-$request.json"
}

wait_status() { # SESSION EXPECTED (space-separated)
  local session="$1" expected=" $2 " status=
  for _ in $(seq 1 300); do
    api GET "/v1/sessions/$session/run" >/dev/null
    status="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("status",""))' "$EVIDENCE/last-body.json" 2>/dev/null || true)"
    case "$expected" in *" $status "*) echo "$status"; return 0 ;; esac
    sleep 0.1
  done
  die "session $session never reached [$2] (last status: ${status:-unknown})"
}

wait_wire_model() { # MODEL COUNT
  local model="$1" want="$2"
  for _ in $(seq 1 300); do
    if MODEL="$model" WANT="$want" FILE="$EVIDENCE/wire-requests.jsonl" python3 - <<'PY'
import json, os, sys
path = os.environ["FILE"]
want = int(os.environ["WANT"])
count = 0
for line in open(path, encoding="utf-8"):
    if not line.strip():
        continue
    try:
        row = json.loads(line)
    except json.JSONDecodeError:
        continue
    if row.get("model") == os.environ["MODEL"]:
        count += 1
sys.exit(0 if count >= want else 1)
PY
    then return 0; fi
    sleep 0.1
  done
  die "the stub never recorded $want request(s) for model $model"
}

# --- fixtures --------------------------------------------------------------
log "creating captain agent (KITE-START-CAPTAIN-MARK) and team"
CAPTAIN_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "id": "agent_start_captain",
    "name": "Start Captain",
    "defaultModel": {"providerId": "kite-e2e", "modelId": "kite-team-slow"},
    "systemPrompt": {"kind": "inline", "text": "KITE-START-CAPTAIN-MARK captain instructions: answer as the team captain."},
    "tools": [{"toolId": "agent", "enabled": True}],
    "teamCli": {"enabled": False},
}))
PY
)"
code="$(api POST /v1/agents "$CAPTAIN_BODY")"
[[ "$code" == "201" ]] || die "create captain agent: HTTP $code $(cat "$EVIDENCE/last-body.json")"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/captain-agent.json"

TEAM_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "id": "team_start",
    "name": "Start Team",
    "captain": {"agentId": "agent_start_captain"},
    "members": [],
    "defaults": {"tools": [], "modelOverride": None},
    "capabilities": {"teamCli": {"enabled": False}},
}))
PY
)"
code="$(api POST /v1/teams "$TEAM_BODY")"
[[ "$code" == "201" ]] || die "create team: HTTP $code $(cat "$EVIDENCE/last-body.json")"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/team-created.json"
code="$(api GET /v1/teams/team_start)"
[[ "$code" == "200" ]] || die "read team: HTTP $code"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/team-read.json"

# The submit gate: an unknown team is refused before it is stored.
code="$(api POST /v1/sessions "$(python3 -c 'import json;print(json.dumps({"sessionId":"00000000-0000-4000-8000-0000000000bb"}))')")"
[[ "$code" == "201" ]] || die "create refusal probe session: HTTP $code"
code="$(api POST /v1/sessions/00000000-0000-4000-8000-0000000000bb/input \
  '{"text":"probe","selection":{"teamId":"team_does_not_exist"}}')"
[[ "$code" == "400" ]] || die "an unknown teamId must be refused at submit: HTTP $code"
grep -q 'team_does_not_exist' "$EVIDENCE/last-body.json" || die "the refusal must name the team id: $(cat "$EVIDENCE/last-body.json")"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/refused-unknown-team.json"

# --- leg A: standard start, model frozen ----------------------------------
log "leg A: standard input selects kite-standard-b"
STD="$(uuidgen | tr '[:upper:]' '[:lower:]')"
new_session "$STD"
send_input "$STD" "Standard turn: answer on the selected model." "p7-standard" \
  '{"providerId":"kite-e2e","modelId":"kite-standard-b"}'
wait_status "$STD" "done" >/dev/null
api GET "/v1/sessions/$STD/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/standard-events.json"

# --- leg B: team start, captain/revision frozen ---------------------------
log "leg B: team-only selection starts the captain on its default model"
TEAM_SESSION="$(uuidgen | tr '[:upper:]' '[:lower:]')"
new_session "$TEAM_SESSION"
send_input "$TEAM_SESSION" "Team turn: answer as the captain." "p7-team" '{"teamId":"team_start"}'
wait_status "$TEAM_SESSION" "done" >/dev/null
api GET "/v1/sessions/$TEAM_SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/team-events.json"

# --- leg C: switching mid-run affects the next run only -------------------
log "leg C: standard(slow) -> queue team -> standard while the team run is live"
SWITCH="$(uuidgen | tr '[:upper:]' '[:lower:]')"
new_session "$SWITCH"
send_input "$SWITCH" "Run 1: standard slow turn." "p7-switch-1" \
  '{"providerId":"kite-e2e","modelId":"kite-slow"}'
wait_status "$SWITCH" "running" >/dev/null
wait_wire_model "kite-slow" 1
log "  run 1 is in flight; queueing the team input"
send_input "$SWITCH" "Run 2: team turn queued while run 1 was live." "p7-switch-2" '{"teamId":"team_start"}'

# The team leg already produced one kite-team-slow request; the second one can
# only exist after run 1 settled and run 2 started. The request is recorded
# before the stub sleeps, so run 2's identity is frozen.
wait_wire_model "kite-team-slow" 2
log "  run 2 is in flight; queueing the standard input"
send_input "$SWITCH" "Run 3: standard turn queued while the team run was live." "p7-switch-3" \
  '{"providerId":"kite-e2e","modelId":"kite-standard-b"}'

# Wait for the third run.finished (all three attempts settle in input order).
for _ in $(seq 1 300); do
  api GET "/v1/sessions/$SWITCH/events" >/dev/null
  count="$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1])); ev=d if isinstance(d,list) else d.get("events",[])
print(sum(1 for e in ev if e.get("kind")=="run.finished"))' "$EVIDENCE/last-body.json" 2>/dev/null || echo 0)"
  [[ "$count" -ge 3 ]] && break
  sleep 0.1
done
cp "$EVIDENCE/last-body.json" "$EVIDENCE/switch-events.json"
wait_status "$SWITCH" "done" >/dev/null

# --- assertions ------------------------------------------------------------
log "asserting the frozen identities and the per-input selections"
python3 - "$EVIDENCE" <<'PY'
import json, sys

out = sys.argv[1]

def load(name):
    return json.load(open(out + "/" + name))

def events_of(name):
    doc = load(name)
    return doc if isinstance(doc, list) else doc.get("events", [])

def summaries(name):
    return [e["payload"] for e in events_of(name) if e.get("kind") == "run.finished"]

def users(name):
    return [e["payload"] for e in events_of(name) if e.get("kind") == "user.message"]

def identity(summary):
    return {
        "status": summary.get("status"),
        "provider": summary.get("provider"),
        "model": summary.get("model"),
        "agentId": summary.get("agentId") or "",
        "teamId": summary.get("teamId") or "",
        "teamRevision": summary.get("teamRevision") or 0,
        "teamHash": summary.get("teamHash") or "",
        "promptHash": summary.get("promptHash") or "",
    }

# Leg A: standard model start.
std = summaries("standard-events.json")
assert len(std) == 1, "standard leg must settle exactly one run, got %d" % len(std)
std_identity = identity(std[0])
assert std_identity["status"] == "done", std_identity
assert std_identity["provider"] == "kite-e2e", std_identity
assert std_identity["model"] == "kite-standard-b", std_identity
assert std_identity["agentId"] == "" and std_identity["teamId"] == "", std_identity
std_user = users("standard-events.json")
assert std_user[-1]["selection"] == {"providerId": "kite-e2e", "modelId": "kite-standard-b"}, std_user[-1]
json.dump(std_identity, open(out + "/standard-summary.json", "w"), ensure_ascii=False, indent=2)

# Leg B: team start; the selection named only the team.
team = summaries("team-events.json")
assert len(team) == 1, "team leg must settle exactly one run, got %d" % len(team)
team_identity = identity(team[0])
team_doc = load("team-read.json")
assert team_identity["status"] == "done", team_identity
assert team_identity["provider"] == "kite-e2e", team_identity
assert team_identity["model"] == "kite-team-slow", team_identity  # captain default, not tenant default
assert team_identity["agentId"] == "agent_start_captain", team_identity
assert team_identity["teamId"] == "team_start", team_identity
assert int(team_identity["teamRevision"]) == int(team_doc.get("revision") or 0) >= 1, (team_identity, team_doc)
assert team_identity["teamHash"] == team_doc.get("hash"), (team_identity, team_doc)
assert team_identity["promptHash"], team_identity
team_user = users("team-events.json")
assert team_user[-1]["selection"] == {"teamId": "team_start"}, team_user[-1]
json.dump(team_identity, open(out + "/team-summary.json", "w"), ensure_ascii=False, indent=2)

# Leg C: three runs; each summary carries its own input's identity.
sw = summaries("switch-events.json")
assert len(sw) == 3, "switch leg must settle three runs, got %d" % len(sw)
run1, run2, run3 = (identity(item) for item in sw)
assert run1["status"] == run2["status"] == run3["status"] == "done", (run1, run2, run3)
# Run 1: standard slow, no binding.
assert run1["model"] == "kite-slow" and run1["agentId"] == "" and run1["teamId"] == "", run1
# Run 2: the team input queued mid-run; the captain/default model answered.
assert run2["model"] == "kite-team-slow", run2
assert run2["agentId"] == "agent_start_captain" and run2["teamId"] == "team_start", run2
assert int(run2["teamRevision"]) == int(team_doc.get("revision") or 0) >= 1, run2
assert run2["teamHash"] == team_doc.get("hash"), run2
# Run 3: the standard input queued mid-team-run; the team binding is gone.
assert run3["model"] == "kite-standard-b", run3
assert run3["agentId"] == "" and run3["teamId"] == "", run3
sw_users = users("switch-events.json")
selections = [p["selection"] for p in sw_users if p.get("requestId", "").startswith("p7-switch-")]
assert selections == [
    {"providerId": "kite-e2e", "modelId": "kite-slow"},
    {"teamId": "team_start"},
    {"providerId": "kite-e2e", "modelId": "kite-standard-b"},
], selections
json.dump({"run1": run1, "run2": run2, "run3": run3},
          open(out + "/switch-summaries.json", "w"), ensure_ascii=False, indent=2)
print("standard summary:", std_identity)
print("team summary:", team_identity)
print("switch run 1:", run1)
print("switch run 2:", run2)
print("switch run 3:", run3)
PY

log "asserting the wire requests"
python3 - "$EVIDENCE" <<'PY'
import json, sys

out = sys.argv[1]
rows = [json.loads(line) for line in open(out + "/wire-requests.jsonl", encoding="utf-8") if line.strip()]

def system_of(row):
    for msg in row.get("messages") or []:
        if msg.get("role") == "system":
            return msg.get("content") or ""
    return ""

assert len(rows) == 5, "expected five provider requests, got %d" % len(rows)
models = [row.get("model") for row in rows]
# The script sends leg A (standard), leg B (team), then leg C's three runs:
# standard slow, team slow, standard. Each leg is a separate session, so the
# requests arrive in that order.
assert models == ["kite-standard-b", "kite-team-slow", "kite-slow", "kite-team-slow", "kite-standard-b"], models
# The switch session's three requests are the last three rows.
switch_rows = rows[2:]
assert [row.get("model") for row in switch_rows] == ["kite-slow", "kite-team-slow", "kite-standard-b"], switch_rows
assert "KITE-START-CAPTAIN-MARK" not in system_of(rows[0]), "standard leg used the captain prompt"
assert "KITE-START-CAPTAIN-MARK" in system_of(rows[1]), "team leg did not use the captain prompt"
assert "KITE-START-CAPTAIN-MARK" not in system_of(switch_rows[0]), "switch run 1 used the captain prompt"
assert "KITE-START-CAPTAIN-MARK" in system_of(switch_rows[1]), "switch run 2 did not use the captain prompt"
assert "KITE-START-CAPTAIN-MARK" not in system_of(switch_rows[2]), "switch run 3 kept the team prompt"
for row in rows:
    assert row.get("authPresent") is True, "the stub saw no Authorization header"
json.dump({
    "models": [row.get("model") for row in rows],
    "switchModels": [row.get("model") for row in switch_rows],
    "captainPrompt": [bool("KITE-START-CAPTAIN-MARK" in system_of(row)) for row in rows],
    "requests": len(rows),
}, open(out + "/wire-assertions.json", "w"), ensure_ascii=False, indent=2)
print("wire assertions: OK (%d requests)" % len(rows))
PY

log "PASS — evidence in $EVIDENCE"
