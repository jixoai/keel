#!/usr/bin/env bash
# Agent/Team runtime E2E (change keel-agent-team-v1, RUNTIME-PLAN §d.1).
#
# What it proves, against a real PostgreSQL database and a real `keel serve`:
#
#   Leg 1 — the run assembly consumes the Agent/Team binding:
#     an input whose selection names a team resolves the captain; the captain's
#     system prompt (KITE-RUNTIME-MARK-7) reaches the provider request; the
#     captain's tool grants filter the tool surface (clock only, plus
#     spawn_subagent because a team run always keeps the delegation primitive);
#     the run settles through the loop and its run.finished freezes
#     agentId/teamId/teamRevision/teamHash/promptHash.
#
#   Leg 2 — a member runs as its own Agent:
#     spawning a member (agentId) commits a child input whose selection carries
#     agentId; the child attempt uses the member's own prompt
#     (KITE-RUNTIME-MARK-8) and the member's own tool grants (echo, and NOT
#     spawn_subagent — agent mode adds nothing).
#
# The provider is a local recording stub: it captures every request body
# (messages + tools) and answers with a valid OpenAI streaming response, so the
# run completes without any upstream credential. No real credential is used and
# nothing is printed.
#
# Isolation: a dedicated database (DROP/CREATE ... WITH (FORCE)) and free
# loopback ports inside the project's dev range (13610-13799). Everything is
# torn down on exit; the evidence directory keeps the raw artifacts.
#
# Usage: scripts/agent-runtime-e2e.sh [--evidence DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
EVIDENCE="${KEEL_AGENT_E2E_EVIDENCE:-$ROOT/openspec/changes/keel-agent-team-v1/evidence/runtime-$STAMP}"
DB="keel_agent_runtime_e2e_$STAMP"
DB="${DB//-/_}"
PORT_RANGE_START=13610
PORT_RANGE_END=13799

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[agent-runtime-e2e] %s\n' "$*"; }
die() { printf '[agent-runtime-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

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

# The test DSN: same server, a dedicated database name.
TEST_DSN="$(KEEL_DSN_VALUE="$DSN" DB_NAME="$DB" python3 - <<'PY'
import os, re
dsn = os.environ["KEEL_DSN_VALUE"]
name = os.environ["DB_NAME"]
print(re.sub(r"/[^/?]+(\?|$)", "/" + name + r"\1", dsn, count=1))
PY
)"

# The PostgreSQL container is the one publishing the DSN's port, so a machine
# running several postgres containers cannot pick the wrong one.
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

# pick_port EXCLUDED: the first free port in the dev range that is neither
# excluded (the caller's own earlier pick — a command substitution runs in a
# subshell, so the choice must be passed back in) nor already bound.
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
  # Port release evidence.
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

# --- recording stub provider ----------------------------------------------
cat > "$EVIDENCE/stub-provider.py" <<'PY'
# The recording stub: captures every /v1/chat/completions request body and
# answers with a valid OpenAI streaming response (the demo's wire shape), so the
# run settles without any upstream credential.
import json, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
BODIES = sys.argv[2]
LOG = sys.argv[3]

REPLY = "KITE-STUB-REPLY 已收到。"

def record(doc):
    with open(BODIES, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

def chunks(model):
    def wrap(delta, finish=None, usage=None):
        out = {
            "id": "chatcmpl-kite-stub",
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
        record({
            "path": self.path,
            "model": doc.get("model"),
            "stream": doc.get("stream"),
            "messages": doc.get("messages"),
            "tools": [
                t.get("function", {}).get("name") for t in (doc.get("tools") or [])
            ],
            "authPresent": bool(auth),
        })
        with open(LOG, "a", encoding="utf-8") as fh:
            fh.write("POST %s model=%s tools=%s\n" % (self.path, doc.get("model"),
                                                      [t.get("function", {}).get("name") for t in (doc.get("tools") or [])]))
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks(doc.get("model") or "stub"))
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
python3 "$EVIDENCE/stub-provider.py" "$STUB_PORT" "$EVIDENCE/wire-requests.jsonl" "$EVIDENCE/stub-provider.log" &
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
      "name": "Kite E2E (recording stub)",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_E2E_KEY"},
      "modelIds": ["kite-e2e-model", "kite-member-model"],
      "visibility": "visible"
    }
  ],
  "selection": {"providerId": "kite-e2e", "modelId": "kite-e2e-model"}
}
JSON

export KITE_E2E_KEY="kite-e2e-not-a-credential"
KEEL_BIN="${KEEL_BIN:-/tmp/keel-agent-e2e}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

# The server reads KEEL_DSN; the dedicated database travels through it.
KEEL_DSN="$TEST_DSN" KEEL_TEST_DSN="$TEST_DSN" KEEL_MODELS_CONFIG="$EVIDENCE/models-config.json" \
  "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$(uuidgen | tr '[:upper:]' '[:lower:]')" \
  --web-dir "$ROOT/web/dist" > "$EVIDENCE/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 100); do
  if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
  kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $EVIDENCE/serve.log)"
  sleep 0.1
done
curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

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

# --- fixtures --------------------------------------------------------------
log "creating captain agent (KITE-RUNTIME-MARK-7) and team"
CAPTAIN_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "id": "agent_kite_captain",
    "name": "Kite Captain",
    "defaultModel": {"providerId": "kite-e2e", "modelId": "kite-e2e-model"},
    "systemPrompt": {"kind": "inline", "text": "KITE-RUNTIME-MARK-7 captain instructions: coordinate the team."},
    "tools": [{"toolId": "clock", "enabled": True}],
    "teamCli": {"enabled": True, "commands": ["member.add"]},
}))
PY
)"
code="$(api POST /v1/agents "$CAPTAIN_BODY")"
[[ "$code" == "201" ]] || die "create captain agent: HTTP $code $(cat "$EVIDENCE/last-body.json")"

log "creating member agent (KITE-RUNTIME-MARK-8)"
MEMBER_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "id": "agent_kite_member",
    "name": "Kite Member",
    "defaultModel": {"providerId": "kite-e2e", "modelId": "kite-member-model"},
    "systemPrompt": {"kind": "inline", "text": "KITE-RUNTIME-MARK-8 member instructions: review the draft."},
    "tools": [{"toolId": "echo", "enabled": True}],
}))
PY
)"
code="$(api POST /v1/agents "$MEMBER_BODY")"
[[ "$code" == "201" ]] || die "create member agent: HTTP $code $(cat "$EVIDENCE/last-body.json")"

TEAM_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "id": "team_kite",
    "name": "Kite Team",
    "captain": {"agentId": "agent_kite_captain"},
    "members": [{"agentId": "agent_kite_member", "label": "reviewer"}],
    "defaults": {"tools": [], "modelOverride": None},
    "capabilities": {"teamCli": {"enabled": True, "commands": ["member.add"]}},
}))
PY
)"
code="$(api POST /v1/teams "$TEAM_BODY")"
[[ "$code" == "201" ]] || die "create team: HTTP $code $(cat "$EVIDENCE/last-body.json")"

# The submit gate: a selection naming an unknown binding is refused before it is
# stored (the same gate an unexecutable model goes through).
code="$(api POST /v1/sessions "$(python3 -c 'import json;print(json.dumps({"sessionId":"00000000-0000-4000-8000-0000000000aa"}))')")"
code="$(api POST /v1/sessions/00000000-0000-4000-8000-0000000000aa/input \
  '{"text":"probe","selection":{"teamId":"team_does_not_exist"}}')"
[[ "$code" == "400" ]] || die "an unknown teamId must be refused at submit: HTTP $code"
grep -q 'team_does_not_exist' "$EVIDENCE/last-body.json" || die "the refusal must name the team id: $(cat "$EVIDENCE/last-body.json")"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/refused-unknown-team.json"

# --- leg 1: team session, captain run -------------------------------------
SESSION="$(uuidgen | tr '[:upper:]' '[:lower:]')"
code="$(api POST /v1/sessions "$(python3 -c 'import json,sys;print(json.dumps({"sessionId":sys.argv[1]}))' "$SESSION")")"
[[ "$code" == "201" ]] || die "create session: HTTP $code $(cat "$EVIDENCE/last-body.json")"

INPUT_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "text": "Captain, delegate the review to the member.",
    "requestId": "kite-captain-turn-1",
    "selection": {"providerId": "kite-e2e", "modelId": "kite-e2e-model", "teamId": "team_kite"},
}))
PY
)"
code="$(api POST "/v1/sessions/$SESSION/input" "$INPUT_BODY")"
[[ "$code" == "201" ]] || die "captain input: HTTP $code $(cat "$EVIDENCE/last-body.json")"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/captain-input.json"

log "waiting for the captain run to settle"
for _ in $(seq 1 200); do
  api GET "/v1/sessions/$SESSION/run" >/dev/null
  status="$(python3 -c 'import json;print(json.load(open("'"$EVIDENCE"'/last-body.json")).get("status",""))')"
  case "$status" in
    done|failed|cancelled) break ;;
  esac
  sleep 0.2
done
[[ "${status:-}" == "done" ]] || die "the captain run settled as ${status:-unknown} (see $EVIDENCE/serve.log)"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/captain-run.json"

api GET "/v1/sessions/$SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/captain-events.json"

python3 - "$EVIDENCE" <<'PY'
import json, sys
ev = json.load(open(sys.argv[1] + "/captain-events.json"))
events = ev if isinstance(ev, list) else ev.get("events", [])
finished = [e for e in events if e.get("kind") == "run.finished"]
assert finished, "no run.finished committed"
summary = finished[-1]["payload"]
for key in ("agentId", "teamId", "teamHash", "promptHash"):
    assert summary.get(key), "run.finished lacks %s: %s" % (key, summary)
assert summary["agentId"] == "agent_kite_captain", summary
assert summary["teamId"] == "team_kite", summary
assert int(summary.get("teamRevision", 0)) >= 1, summary
user = [e for e in events if e.get("kind") == "user.message"][0]["payload"]
assert user["selection"]["teamId"] == "team_kite", user
json.dump(summary, open(sys.argv[1] + "/captain-summary.json", "w"), ensure_ascii=False, indent=2)
print("captain run.finished frozen binding:", {k: summary.get(k) for k in
      ("agentId", "teamId", "teamRevision", "teamHash", "promptHash")})
PY

# --- leg 2: member spawn (its own agent config) ---------------------------
log "spawning the member through the sub-agent API"
SPAWN_BODY="$(python3 - <<'PY'
import json
print(json.dumps({
    "task": "Review the draft and report back.",
    "agentId": "agent_kite_member",
    "idempotencyKey": "kite-member-review-1",
    "wait": False,
}))
PY
)"
code="$(api POST "/v1/sessions/$SESSION/subagents" "$SPAWN_BODY")"
[[ "$code" == "202" ]] || die "spawn member: HTTP $code $(cat "$EVIDENCE/last-body.json")"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/member-spawn.json"
MEMBER_SESSION="$(python3 -c 'import json;print(json.load(open("'"$EVIDENCE"'/member-spawn.json"))["subagent"]["sessionId"])')"

log "waiting for the member run to settle ($MEMBER_SESSION)"
for _ in $(seq 1 200); do
  api GET "/v1/sessions/$MEMBER_SESSION/run" >/dev/null
  status="$(python3 -c 'import json;print(json.load(open("'"$EVIDENCE"'/last-body.json")).get("status",""))')"
  case "$status" in
    done|failed|cancelled) break ;;
  esac
  sleep 0.2
done
[[ "${status:-}" == "done" ]] || die "the member run settled as ${status:-unknown}"
cp "$EVIDENCE/last-body.json" "$EVIDENCE/member-run.json"
api GET "/v1/sessions/$MEMBER_SESSION/events" >/dev/null
cp "$EVIDENCE/last-body.json" "$EVIDENCE/member-events.json"

python3 - "$EVIDENCE" <<'PY'
import json, sys
ev = json.load(open(sys.argv[1] + "/member-events.json"))
events = ev if isinstance(ev, list) else ev.get("events", [])
user = [e for e in events if e.get("kind") == "user.message"][0]["payload"]
assert user["selection"]["agentId"] == "agent_kite_member", user
summary = [e for e in events if e.get("kind") == "run.finished"][-1]["payload"]
assert summary.get("agentId") == "agent_kite_member", summary
print("member input selection:", user["selection"])
PY

# --- wire assertions -------------------------------------------------------
log "asserting the wire requests"
python3 - "$EVIDENCE" <<'PY'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1] + "/wire-requests.jsonl") if l.strip()]
assert len(rows) >= 2, "expected a captain request and a member request, got %d" % len(rows)

def system_of(row):
    for msg in row.get("messages") or []:
        if msg.get("role") == "system":
            return msg.get("content") or ""
    return ""

captain = [r for r in rows if "KITE-RUNTIME-MARK-7" in system_of(r)]
member = [r for r in rows if "KITE-RUNTIME-MARK-8" in system_of(r)]
assert captain, "no wire request carried the captain prompt (KITE-RUNTIME-MARK-7)"
assert member, "no wire request carried the member prompt (KITE-RUNTIME-MARK-8)"

captain_tools = captain[-1]["tools"]
member_tools = member[-1]["tools"]
print("captain wire tools:", captain_tools)
print("member wire tools:", member_tools)
assert captain_tools == ["clock", "spawn_subagent"], captain_tools
assert member_tools == ["echo"], member_tools
# The deployment prompt must NOT be the base instruction of a bound run, while the
# dynamic sections the host appends (plugin standard) must still be present: the
# binding replaces the preamble, it does not drop everything around it.
for label, row in (("captain", captain[-1]), ("member", member[-1])):
    text = system_of(row)
    assert "You are an AI employee" not in text, label + " kept the deployment preamble"
    assert "<plugin_standard>" in text, label + " lost the host's dynamic prompt sections"
json.dump({
    "captainTools": captain_tools,
    "memberTools": member_tools,
    "captainSystemHead": system_of(captain[-1])[:160],
    "memberSystemHead": system_of(member[-1])[:160],
    "requests": len(rows),
}, open(sys.argv[1] + "/wire-assertions.json", "w"), ensure_ascii=False, indent=2)
print("wire assertions: OK")
PY

log "PASS — evidence in $EVIDENCE"
