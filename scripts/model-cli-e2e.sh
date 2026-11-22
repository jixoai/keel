#!/usr/bin/env bash
# Model CLI E2E (change keel-agent-team-v1, P1 model-plugin): `keel model
# provider list|show|add|update|remove|test` and `keel model catalog` against a
# real `keel serve` and a dedicated PostgreSQL database.
#
# What it proves:
#   1. list/show read the server's resolved projection; show prints credential
#      *references* only (never a value).
#   2. add/update/remove edit the tenant models document through the revision-
#      guarded PUT /v1/settings/models, and list/show observe the writes.
#   3. test sends one real minimal upstream model-list request through the
#      server's credential resolution: ready against the recording stub,
#      needs_auth without a resolvable credential (nothing sent), unreachable
#      for a dead endpoint. A success is never reported without the request.
#   4. catalog reads the resolved catalog and --refresh prints the live upstream
#      lineup discovered by the same probe.
#   5. a rejected write renders the server's field path; a referenced provider
#      removal renders 409 provider_in_use and the referring Bot.
#   6. the credential value never appears in any CLI output, evidence file or
#      the serve log; the document stores references only.
#
# Isolation: a dedicated database (DROP/CREATE ... WITH (FORCE)), loopback
# ports inside the project's dev range, a local recording stub provider whose
# credential is a fake value. Every process is reaped and the database dropped
# on exit.
#
# Usage: scripts/model-cli-e2e.sh [--evidence DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${KEEL_MODEL_CLI_E2E_EVIDENCE:-$ROOT/openspec/changes/keel-agent-team-v1/evidence/model-cli-2026-11-21}"
DB="keel_model_cli_e2e_$(date +%Y%m%d_%H%M%S)"
PORT_RANGE_START=13660
PORT_RANGE_END=13799

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[model-cli-e2e] %s\n' "$*"; }
die() { printf '[model-cli-e2e] FAIL: %s\n' "$*" >&2; exit 1; }

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
DEAD_PORT="$(pick_port "$SERVE_PORT $STUB_PORT")"

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
    KEEL_SERVE_PASSWORD) : ;; # retired; never forwarded to the server or CLI
  esac
done < <(grep -E '^(KEEL_[A-Z0-9_]+)=|^KEEL_DSN=' "$ENV_FILE")
unset KEEL_SERVE_PASSWORD KEEL_AGENT_TOKEN
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
  [[ -n "${KEEL_E2E_JAR:-}" ]] && rm -f "$KEEL_E2E_JAR" "$EVIDENCE/csrf.txt" "$EVIDENCE/auth.json" 2>/dev/null || true
  docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1 || true
  sleep 0.5
  {
    echo "# port release check"
    for p in "$SERVE_PORT" "$STUB_PORT" "$DEAD_PORT"; do
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

# --- recording stub provider -----------------------------------------------
# The stub returns a live catalog that differs from the resolved document (a
# model the document does not declare), so --refresh proves it is a real fetch.
cat > "$EVIDENCE/stub-provider.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
LOG = sys.argv[2]
EXPECTED = os.environ.get("STUB_EXPECTED_KEY", "")

LIVE_MODELS = ["stub-model-a", "stub-model-b", "stub-model-live"]

def record(doc):
    with open(LOG, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        authorized = self.headers.get("Authorization") == "Bearer " + EXPECTED
        record({"method": "GET", "path": self.path, "authorized": authorized})
        if self.path.rstrip("/") != "/v1/models":
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if not authorized:
            self.send_response(401)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        body = json.dumps({"data": [{"id": m} for m in LIVE_MODELS]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PY

: > "$EVIDENCE/stub-wire.jsonl"
export STUB_EXPECTED_KEY="kite-model-e2e-$(uuidgen | tr '[:upper:]' '[:lower:]')"
export KITE_MODEL_KEY="$STUB_EXPECTED_KEY"
unset KITE_MISSING_KEY
python3 "$EVIDENCE/stub-provider.py" "$STUB_PORT" "$EVIDENCE/stub-wire.jsonl" &
STUB_PID=$!
for _ in $(seq 1 60); do port_free "$STUB_PORT" || break; sleep 0.1; done
port_free "$STUB_PORT" && die "the stub provider did not start"

cat > "$EVIDENCE/models-config.json" <<JSON
{
  "schemaVersion": 1,
  "providers": [
    {
      "id": "stub",
      "name": "Stub Provider",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_MODEL_KEY"},
      "modelIds": ["stub-model-a", "stub-model-b"],
      "visibility": "visible"
    },
    {
      "id": "noauth",
      "name": "No Auth Provider",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$STUB_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_MISSING_KEY"},
      "modelIds": ["stub-model-a"]
    },
    {
      "id": "dead",
      "name": "Dead Provider",
      "api": {"type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$DEAD_PORT/v1"},
      "access": {"type": "api-key", "env": "KITE_MODEL_KEY"},
      "modelIds": ["stub-model-a"]
    }
  ],
  "selection": {"providerId": "stub", "modelId": "stub-model-a"}
}
JSON

KEEL_BIN="${KEEL_BIN:-/tmp/keel-model-cli-e2e}"
log "building $KEEL_BIN"
( cd "$ROOT" && go build -o "$KEEL_BIN" ./cmd/keel )

TENANT="$(uuidgen | tr '[:upper:]' '[:lower:]')"
# The probe must see exactly the credentials this script controls: an inherited
# process-wide key would turn the needs_auth case into a success.
unset KEEL_API_KEY KITE_MISSING_KEY KEEL_AGENT_TOKEN
KEEL_DSN="$TEST_DSN" KEEL_SERVE_TENANT="$TENANT" \
  KEEL_SECRET_KEY="$(openssl rand -hex 32)" \
  KEEL_MODELS_CONFIG="$EVIDENCE/models-config.json" \
  "$KEEL_BIN" serve --addr "127.0.0.1:$SERVE_PORT" --tenant "$TENANT" \
  > "$EVIDENCE/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 120); do
  if curl -sf -m 1 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null 2>&1; then break; fi
  kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup (see $EVIDENCE/serve.log)"
  sleep 0.1
done
curl -sf -m 2 "http://127.0.0.1:$SERVE_PORT/v1/health" >/dev/null || die "keel serve did not become healthy"

export KEEL_SERVE_ADDR="127.0.0.1:$SERVE_PORT"

# The fresh database has no super admin: bootstrap one account and let the CLI
# log in with KEEL_ACCOUNT_USERNAME/PASSWORD (the password flag is refused).
source "$ROOT/scripts/lib/keel-auth.sh"
keel_auth_provision "http://127.0.0.1:$SERVE_PORT" "$KEEL_BIN" "$TEST_DSN" "$TENANT" "$EVIDENCE"
export KEEL_SERVE_ADDR="127.0.0.1:$SERVE_PORT"

# api METHOD PATH [BODY] -> body in last-body.json, status on stdout. The
# session cookie and CSRF token come from keel_auth_provision.
api() {
  local method="$1" path="$2" body="${3:-}"
  local -a args=(-sS -b "$KEEL_E2E_JAR" -c "$KEEL_E2E_JAR" -X "$method"
    -H "X-Keel-CSRF: $KEEL_E2E_CSRF")
  local base="http://127.0.0.1:$SERVE_PORT"
  if [[ -n "$body" ]]; then
    curl "${args[@]}" "$base$path" -H 'Content-Type: application/json' \
      --data-binary "$body" -o "$EVIDENCE/last-body.json" -w '%{http_code}'
  else
    curl "${args[@]}" "$base$path" -o "$EVIDENCE/last-body.json" -w '%{http_code}'
  fi
}

# cli NAME ARGS... -> stdout in NAME.out, stderr in NAME.err, exit in NAME.code
cli() {
  local name="$1"; shift
  set +e
  "$KEEL_BIN" "$@" > "$EVIDENCE/$name.out" 2> "$EVIDENCE/$name.err"
  local code=$?
  set -e
  echo "$code" > "$EVIDENCE/$name.code"
  return 0
}

expect_code() { # NAME WANT
  local got; got="$(cat "$EVIDENCE/$1.code")"
  [[ "$got" == "$2" ]] || die "$1 exit = $got, want $2 (stderr: $(cat "$EVIDENCE/$1.err" 2>/dev/null || true))"
}
expect_out() { # NAME NEEDLE
  grep -q -- "$2" "$EVIDENCE/$1.out" || die "$1 stdout lacks \"$2\": $(cat "$EVIDENCE/$1.out")"
}
expect_err() { # NAME NEEDLE
  grep -q -- "$2" "$EVIDENCE/$1.err" || die "$1 stderr lacks \"$2\": $(cat "$EVIDENCE/$1.err")"
}

# --- 1. list / show ---------------------------------------------------------
log "reading the provider registry"
cli list-json model provider list --json
expect_code list-json 0
python3 - "$EVIDENCE/list-json.out" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
ids = {p["providerId"] for p in body["providers"]}
missing = {"stub", "noauth", "dead"} - ids
assert not missing, (missing, sorted(ids))
print("configured providers present:", sorted({"stub", "noauth", "dead"} & ids))
PY
cli list-table model provider list
expect_code list-table 0
for want in stub noauth dead; do expect_out list-table "$want"; done
cli list-filter model provider list --type image
expect_code list-filter 0
for gone in stub noauth dead; do
  grep -q "$gone" "$EVIDENCE/list-filter.out" && die "--type image kept llm provider $gone"
done
cli show model provider show stub
expect_code show 0
python3 - "$EVIDENCE/show.out" <<'PY'
import json, sys
view = json.load(open(sys.argv[1]))
assert view["providerId"] == "stub", view
assert view["access"] == {"type": "api-key", "env": "KITE_MODEL_KEY"}, view["access"]
assert view["api"]["baseUrl"].endswith("/v1"), view["api"]
assert len(view["models"]) == 2, view["models"]
print("shown provider:", view["providerId"], view["access"])
PY
cli show-missing model provider show nope
expect_code show-missing 1
expect_err show-missing "does not serve a provider"

# --- 2. catalog -------------------------------------------------------------
log "reading the model catalog"
cli catalog-resolved model catalog stub --json
expect_code catalog-resolved 0
python3 - "$EVIDENCE/catalog-resolved.out" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
assert body["providerId"] == "stub" and body["source"] == "resolved", body
ids = sorted(m["modelId"] for m in body["models"])
assert ids == ["stub-model-a", "stub-model-b"], ids
print("resolved catalog:", ids)
PY
cli catalog-refresh model catalog stub --refresh --json
expect_code catalog-refresh 0
python3 - "$EVIDENCE/catalog-refresh.out" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
assert body["providerId"] == "stub" and body["source"] == "upstream", body
assert sorted(body["models"]) == ["stub-model-a", "stub-model-b", "stub-model-live"], body["models"]
print("upstream catalog:", sorted(body["models"]))
PY
cli catalog-refresh-no-id model catalog --refresh
expect_code catalog-refresh-no-id 2
expect_err catalog-refresh-no-id "needs a provider id"
cli catalog-missing model catalog nope
expect_code catalog-missing 1
expect_err catalog-missing "does not serve a provider"

# --- 3. provider test: ready / needs_auth / unreachable ---------------------
log "probing providers"
: > "$EVIDENCE/stub-wire.jsonl"
cli test-ready model provider test stub
expect_code test-ready 0
for want in "provider stub (llm)" "status ready" "latency" "models 3" "ok: the provider answered"; do
  expect_out test-ready "$want"
done
python3 - "$EVIDENCE/stub-wire.jsonl" <<'PY'
import json, sys
requests = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
assert any(r["path"].rstrip("/") == "/v1/models" and r["authorized"] for r in requests), requests
print("stub saw an authorized model-list request")
PY
cli test-json model provider test stub --json
expect_code test-json 0
python3 - "$EVIDENCE/test-json.out" <<'PY'
import json, sys
probe = json.load(open(sys.argv[1]))
assert probe["providerId"] == "stub" and probe["ok"] is True, probe
assert probe["status"] == "ready" and probe["modelCount"] == 3, probe
assert "latencyMs" in probe, probe
print("probe json:", probe["status"], probe["modelCount"])
PY

: > "$EVIDENCE/stub-wire.jsonl"
cli test-needs-auth model provider test noauth
expect_code test-needs-auth 1
for want in "status needs_auth" "access.env" "not ok:"; do expect_out test-needs-auth "$want"; done
[[ ! -s "$EVIDENCE/stub-wire.jsonl" ]] || die "a needs_auth probe still reached the upstream"

cli test-unreachable model provider test dead
expect_code test-unreachable 1
for want in "status unreachable" "not ok:"; do expect_out test-unreachable "$want"; done

# --- 4. add / update / remove ----------------------------------------------
log "writing the tenant models document"
cli add-extra model provider add extra --type llm \
  --name "Extra Provider" --api-type openai-chat-completions \
  --base-url "http://127.0.0.1:$STUB_PORT/v1" --env KITE_MODEL_KEY \
  --model stub-model-a --visibility visible
expect_code add-extra 0
expect_out add-extra "added provider extra"
cli show-extra model provider show extra
expect_code show-extra 0
python3 - "$EVIDENCE/show-extra.out" <<'PY'
import json, sys
view = json.load(open(sys.argv[1]))
assert view["providerId"] == "extra" and view["name"] == "Extra Provider", view
assert view["access"]["env"] == "KITE_MODEL_KEY", view["access"]
print("added provider visible:", view["providerId"])
PY

cli update-extra model provider update extra --name "Extra Renamed"
expect_code update-extra 0
expect_out update-extra "updated provider extra"
cli show-extra-renamed model provider show extra
expect_code show-extra-renamed 0
python3 - "$EVIDENCE/show-extra-renamed.out" <<'PY'
import json, sys
view = json.load(open(sys.argv[1]))
assert view["name"] == "Extra Renamed", view
print("updated provider name:", view["name"])
PY

cli hide-extra model provider update extra --visibility hidden
expect_code hide-extra 0
cli list-after-hide model provider list --json
expect_code list-after-hide 0
python3 - "$EVIDENCE/list-after-hide.out" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
ids = [p["providerId"] for p in body["providers"]]
assert "extra" not in ids, ids
print("hidden provider left the projection (providers:", len(ids), ")")
PY
cli show-hidden model provider show extra
expect_code show-hidden 1
expect_err show-hidden "does not serve a provider"

# A referenced provider cannot be removed: the server answers 409
# provider_in_use with the referring Bot (the provider registry's deletion
# protection scans bots.default_model, not the retired agent registry).
cli add-guarded model provider add guarded --type llm --name "Guarded Provider" \
  --api-type openai-chat-completions --base-url "http://127.0.0.1:$STUB_PORT/v1" \
  --env KITE_MODEL_KEY --model stub-model-a
expect_code add-guarded 0

role_code="$(api POST /v1/roles '{"name":"Model Guard Role"}')"
[[ "$role_code" == "201" ]] || die "create role: HTTP $role_code $(cat "$EVIDENCE/last-body.json")"
ROLE_ID="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["role"]["roleId"])' "$EVIDENCE/last-body.json")"
profile_body="$(ACCOUNT_ID="$KEEL_E2E_ACCOUNT_ID" ROLE_ID="$ROLE_ID" python3 -c '
import json, os
print(json.dumps({"accountId": os.environ["ACCOUNT_ID"], "roleId": os.environ["ROLE_ID"]}))')"
profile_code="$(api POST /v1/profiles "$profile_body")"
[[ "$profile_code" == "201" ]] || die "create profile: HTTP $profile_code $(cat "$EVIDENCE/last-body.json")"
PROFILE_ID="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["profile"]["profileId"])' "$EVIDENCE/last-body.json")"
bot_body="$(PROFILE_ID="$PROFILE_ID" python3 -c '
import json, os
print(json.dumps({
    "botId": "bot_model_guard", "name": "Model Guard",
    "profileId": os.environ["PROFILE_ID"], "modelKind": "text",
    "defaultModel": {"providerId": "guarded", "modelId": "stub-model-a"},
    "harness": {"tools": [], "context": {"instruction": "", "permissions": []}},
}))')"
bot_code="$(api POST /v1/bots "$bot_body")"
[[ "$bot_code" == "201" ]] || die "create bot: HTTP $bot_code $(cat "$EVIDENCE/last-body.json")"

cli remove-guarded model provider remove guarded
expect_code remove-guarded 1
for want in "409 provider_in_use" "bot bot_model_guard Model Guard" "defaultModel"; do
  expect_err remove-guarded "$want"
done

# Repoint the Bot, then the provider is no longer referenced and the removal
# succeeds. (Bots archive rather than hard-delete, and an archived Bot still
# counts as a reference, so deleting the Bot would not free the provider.)
repoint_code="$(api PATCH /v1/bots/bot_model_guard \
  '{"defaultModel":{"providerId":"stub","modelId":"stub-model-a"}}')"
[[ "$repoint_code" == "200" ]] || die "repoint bot: HTTP $repoint_code $(cat "$EVIDENCE/last-body.json")"
cli remove-guarded-after model provider remove guarded
expect_code remove-guarded-after 0
expect_out remove-guarded-after "removed provider guarded"

archive_code="$(api POST /v1/bots/bot_model_guard/archive '{}')"
[[ "$archive_code" == "200" ]] || die "archive bot: HTTP $archive_code $(cat "$EVIDENCE/last-body.json")"

# A rejected settings write renders the server's field path.
cli add-broken model provider add broken --type llm --name "Broken" \
  --api-type nope --base-url "https://broken.example.test/v1" --env KITE_MODEL_KEY
expect_code add-broken 1
for want in "server refused (400)" "api.type" "must be one of"; do expect_err add-broken "$want"; done

cli remove-extra model provider remove extra
expect_code remove-extra 0
expect_out remove-extra "removed provider extra"
cli list-final model provider list --json
expect_code list-final 0
python3 - "$EVIDENCE/list-final.out" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
ids = [p["providerId"] for p in body["providers"]]
assert "extra" not in ids and "guarded" not in ids, ids
print("final providers:", ids)
PY

# --- 5. database truth ------------------------------------------------------
docker exec "$PG_CTR" psql -U postgres -d "$DB" -At -c \
  "SELECT revision, document::text FROM model_settings" > "$EVIDENCE/model-settings-dump.txt"
grep -q '"providers"' "$EVIDENCE/model-settings-dump.txt" || die "the tenant models document was not stored"
if grep -Fq "$STUB_EXPECTED_KEY" "$EVIDENCE/model-settings-dump.txt"; then
  die "the stored document carries the credential value"
fi
python3 - "$EVIDENCE/model-settings-dump.txt" <<'PY'
import json, sys
line = open(sys.argv[1]).read().strip()
revision, document = line.split("|", 1)
doc = json.loads(document)
for provider in doc.get("providers", []):
    access = provider.get("access") or {}
    assert "env" in access or "ref" in access, provider
print("stored provider references:", [(p["id"], p["access"]) for p in doc.get("providers", [])])
PY

# --- 6. credential non-leak sweep ------------------------------------------
if grep -rFq "$STUB_EXPECTED_KEY" "$EVIDENCE"; then
  die "the credential value leaked into the evidence tree"
fi
if grep -Fq "$STUB_EXPECTED_KEY" "$EVIDENCE/serve.log" 2>/dev/null; then
  die "the credential value leaked into the serve log"
fi
echo "credential value absent from all CLI outputs, evidence files and the serve log" \
  > "$EVIDENCE/credential-non-leak.txt"

log "PASS: model CLI E2E"
