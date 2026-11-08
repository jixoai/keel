#!/usr/bin/env bash
# Real-model E2E: prove the loop completes one real round trip —
# input -> model -> tool round trip (echo) -> final answer — against a real
# OpenAI-compatible or Anthropic endpoint.
#
# What it does (all self-contained, no kernel changes):
#   1. resolves the real provider from the environment (KEEL_MODEL etc., or the
#      project's AI_MODEL / A6API_* convention; optional key-name-only env file);
#   2. creates a dedicated database (name contains e2e) in the dockerized
#      Postgres, DROP/CREATE ... WITH (FORCE), and asserts it is empty;
#   3. builds and starts cmd/runtime on a free local port (never 13610-13799),
#      proving port ownership through the health endpoint's pid;
#   4. provisions a tenant + session, submits one input that explicitly asks
#      the model to call the echo tool, waits for the terminal answer;
#   5. asserts the committed event sequence:
#        user.message -> assistant.message(tool_use, toolCall=echo)
#        -> tool.result(toolCallId match) -> assistant.message(end_turn)
#      plus contiguous seqs, matching input seq, non-empty final text and
#      run status done;
#   6. --failure-auth: same flow with a deliberately invalid key and asserts the
#      model failure is visible (stopReason=error/errorMessage or run failed)
#      while the key literal never appears in events or the runtime log.
#
# Credentials are read from the environment only. They are never echoed, never
# written to a file and never printed in the summary. If KEEL_MODEL/
# KEEL_BASE_URL/KEEL_API_KEY (or the project's AI_MODEL / A6API_* / S2API_*
# convention) are absent, the script may extract JUST those names from an
# existing env file; the file is parsed line-by-line (never sourced/executed) and
# only the variable names are reported.
#
# KEEL_E2E_MODEL_REWRITE=<model-id> starts a local rewriter in front of the
# upstream endpoint. It exists for ONE known wiring bug: internal/servicehost
# stores provider.ResolveProvider's second return value (the provider NAME) as
# the model id, so the wire request carries "model":"openai" instead of the
# configured model (see docs/REAL-MODEL-E2E.md §5). The rewriter restores the
# configured id and logs the rewrite; the loop itself still talks to the real
# gateway. Drop the flag once the wiring is fixed.
#
# Usage:
#   scripts/real-model-e2e.sh                 # success round trip
#   scripts/real-model-e2e.sh --with-failure  # success + 401 failure path
#   scripts/real-model-e2e.sh --failure-auth  # 401 failure path only
#   scripts/real-model-e2e.sh --check         # resolve config only, no network
#
# Exit codes: 0 pass; 1 assertion/runtime failure; 2 blocked (credentials or
# dependencies missing).
#
# Evidence (raw, /tmp only, never committed):
#   $KEEL_E2E_EVIDENCE_DIR/runtime.log, events.json, summary.json,
#   success-{tenant,session,input}.json, rewrite-proxy.log (when the harness
#   workaround is on)
#   $KEEL_E2E_EVIDENCE_DIR/failure-runtime.log, failure-events.json,
#   failure-summary.json, failure-{tenant,session,input}.json
#
# Environment knobs: see docs/REAL-MODEL-E2E.md §3.
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MODE=success
for arg in "$@"; do
  case "$arg" in
    --check) MODE=check ;;
    --failure-auth) MODE=failure ;;
    --with-failure) MODE=both ;;
    -h|--help)
      sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown argument: $arg (try --help)" >&2; exit 2 ;;
  esac
done

say()  { printf '%s\n' "$*"; }
# Progress goes to stderr: helper functions such as ensure_binary print their
# result on stdout inside $(...), so progress must never share that stream.
info() { printf '[e2e] %s\n' "$*" >&2; }
die()  { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
blocked() { printf 'BLOCKED: %s\n' "$*" >&2; exit 2; }

# --- credential resolution -------------------------------------------------

# set_if_unset NAME VALUE — never overrides an existing environment value.
set_if_unset() {
  local name="$1" value="$2"
  [ -n "${value:-}" ] || return 0
  if [ -z "$(printenv "$name" 2>/dev/null || true)" ]; then
    export "$name=$value"
  fi
}

# load_env_file FILE — extract only the known variable names. The file is read
# line by line and is NEVER sourced, so a hostile line cannot run anything.
load_env_file() {
  local file="$1" line name value
  [ -f "$file" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      A6API_BASE_URL=*|A6API_KEY=*|AI_MODEL=*|S2API_BASE_URL=*|S2API_KEY=*|KEEL_MODEL=*|KEEL_BASE_URL=*|KEEL_API_KEY=*|KEEL_PROTOCOL=*|KEEL_PROVIDER=*)
        name="${line%%=*}"
        value="${line#*=}"
        # Trim one layer of matching quotes and trailing CR.
        value="${value%$'\r'}"
        case "$value" in
          \"*\") value="${value#\"}"; value="${value%\"}" ;;
          \'*\') value="${value#\'}"; value="${value%\'}" ;;
        esac
        set_if_unset "$name" "$value"
        ;;
    esac
  done < "$file"
}

super_root() {
  local gitdir
  gitdir="$(git -C "$ROOT" rev-parse --absolute-git-dir 2>/dev/null || true)"
  case "$gitdir" in
    */.git/modules/*) printf '%s\n' "${gitdir%%/.git/modules/*}" ;;
    */.git)           printf '%s\n' "${gitdir%/.git}" ;;
    *)                printf '%s\n' "$ROOT" ;;
  esac
}

ENV_FILE_USED=""
resolve_credentials() {
  local candidates=() file
  if [ -n "${KEEL_E2E_ENV_FILE:-}" ]; then
    [ -f "$KEEL_E2E_ENV_FILE" ] || blocked "KEEL_E2E_ENV_FILE=$KEEL_E2E_ENV_FILE does not exist"
    candidates+=("$KEEL_E2E_ENV_FILE")
  else
    candidates+=("$ROOT/.env.personal")
    candidates+=("$(super_root)/.env.personal")
    candidates+=("$(super_root)/fde2e-omni-v1/.env")
  fi
  for file in "${candidates[@]}"; do
    [ -n "$file" ] || continue
    [ -f "$file" ] || continue
    load_env_file "$file"
    if [ -n "${KEEL_MODEL:-}${KEEL_BASE_URL:-}${KEEL_API_KEY:-}${AI_MODEL:-}${A6API_BASE_URL:-}${A6API_KEY:-}${S2API_BASE_URL:-}${S2API_KEY:-}" ]; then
      ENV_FILE_USED="$file"
      break
    fi
  done

  MODEL="${KEEL_MODEL:-${AI_MODEL:-}}"
  # Gateway family selection: explicit KEEL_* wins; otherwise the project's
  # A6API convention, falling back to the alternate S2API gateway (whose base
  # URL has no version segment, while the driver appends /chat/completions).
  local gateway="${KEEL_E2E_GATEWAY:-auto}" family_base="" family_key="" family_needs_v1=0
  GATEWAY_USED="$gateway"
  case "$gateway" in
    a6api)
      family_base="${A6API_BASE_URL:-}"
      family_key="${A6API_KEY:-}"
      ;;
    s2api)
      family_base="${S2API_BASE_URL:-}"
      family_key="${S2API_KEY:-}"
      family_needs_v1=1
      ;;
    auto)
      if [ -n "${A6API_BASE_URL:-}${A6API_KEY:-}" ]; then
        family_base="${A6API_BASE_URL:-}"
        family_key="${A6API_KEY:-}"
      else
        family_base="${S2API_BASE_URL:-}"
        family_key="${S2API_KEY:-}"
        family_needs_v1=1
      fi
      ;;
    *) blocked "unsupported KEEL_E2E_GATEWAY=$gateway (want auto|a6api|s2api)" ;;
  esac
  if [ "$family_needs_v1" = "1" ]; then
    case "$family_base" in
      */v1|*/v1/*) ;;
      "") ;;
      *) family_base="${family_base%/}/v1" ;;
    esac
  fi
  BASE_URL="${KEEL_BASE_URL:-$family_base}"
  API_KEY="${KEEL_API_KEY:-$family_key}"
  PROVIDER="${KEEL_PROVIDER:-}"
  PROTOCOL="${KEEL_PROTOCOL:-}"
  # Explicit opt-in workaround for the servicehost model-id wiring bug: rewrite
  # the outbound model id to the configured one. "1"/"auto" means $MODEL.
  REWRITE_MODEL="${KEEL_E2E_MODEL_REWRITE:-}"
  case "$REWRITE_MODEL" in
    1|auto|yes) REWRITE_MODEL="$MODEL" ;;
    0) REWRITE_MODEL="" ;;
  esac
  # OpenAI-compatible is the project's test-model default; anthropic may omit
  # the base URL, and an explicit provider must not be paired with a guessed
  # protocol (ResolveNamedProvider rejects a conflict).
  if [ -z "$PROTOCOL" ] && [ -z "$PROVIDER" ]; then
    PROTOCOL=openai
  fi

  [ -n "$MODEL" ] || blocked "no real model configured: set KEEL_MODEL (or AI_MODEL), KEEL_BASE_URL, KEEL_API_KEY"
  [ -n "$API_KEY" ] || blocked "no API credential: set KEEL_API_KEY (or A6API_KEY/S2API_KEY); it is never written to disk"
  case "$PROTOCOL" in
    ""|openai|openai/chat|openai/resp_api|anthropic) ;;
    *) blocked "unsupported KEEL_PROTOCOL=$PROTOCOL (want openai|openai/resp_api|anthropic)" ;;
  esac
  case "$PROTOCOL" in
    openai|openai/chat|openai/resp_api)
      [ -n "$BASE_URL" ] || blocked "KEEL_PROTOCOL=$PROTOCOL requires KEEL_BASE_URL (e.g. https://api.a6api.com/v1)" ;;
  esac
  if [ -n "$REWRITE_MODEL" ] && [ -z "$BASE_URL" ]; then
    blocked "KEEL_E2E_MODEL_REWRITE requires an explicit base URL (KEEL_BASE_URL / A6API_BASE_URL / S2API_BASE_URL)"
  fi
}

endpoint_host() {
  BASE_URL="$BASE_URL" python3 -c 'import os,sys,urllib.parse as u; p=u.urlparse(os.environ["BASE_URL"]); print((p.scheme+"://"+p.netloc) if p.netloc else "(default provider endpoint)")'
}

print_config() {
  local key_state="absent"
  [ -n "$API_KEY" ] && key_state="present (value not shown)"
  say "config:"
  say "  model      = $MODEL"
  say "  protocol   = ${PROTOCOL:-<unset>}"
  say "  provider   = ${PROVIDER:-<auto>}"
  say "  gateway    = ${GATEWAY_USED:-auto}"
  say "  endpoint   = $(endpoint_host)"
  say "  credential = $key_state"
  if [ -n "$REWRITE_MODEL" ]; then
    say "  workaround = local model-id rewriter -> $REWRITE_MODEL (servicehost wiring bug; see docs §5)"
  fi
  if [ -n "$ENV_FILE_USED" ]; then
    say "  env file   = $ENV_FILE_USED (names only; never printed values)"
  fi
}

# --- infrastructure helpers ------------------------------------------------

now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

pick_port() {
  if [ -n "${KEEL_E2E_PORT:-}" ]; then
    case "$KEEL_E2E_PORT" in
      ''|*[!0-9]*) die "KEEL_E2E_PORT must be numeric (got $KEEL_E2E_PORT)" ;;
    esac
    if [ "$KEEL_E2E_PORT" -ge 13610 ] && [ "$KEEL_E2E_PORT" -le 13799 ]; then
      die "KEEL_E2E_PORT=$KEEL_E2E_PORT is inside the reserved 13610-13799 tunnel range; pick another port"
    fi
    port_free "$KEEL_E2E_PORT" || die "KEEL_E2E_PORT=$KEEL_E2E_PORT is already in use"
    printf '%s\n' "$KEEL_E2E_PORT"
    return
  fi
  python3 - <<'PY'
import random, socket
for _ in range(400):
    port = random.randint(18000, 32000)
    if 13610 <= port <= 13799:
        continue
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", port))
        s.close()
        print(port)
        break
    except OSError:
        s.close()
else:
    raise SystemExit("no free port found")
PY
}

port_free() {
  python3 - "$1" <<'PY'
import socket, sys
port = int(sys.argv[1])
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("127.0.0.1", port))
    print("free")
except OSError:
    print("used")
finally:
    s.close()
PY
}

port_owner_pid() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}'
}

# post_json URL OUTFILE DATA [extra curl args...] — prints the HTTP status.
post_json() {
  local url="$1" out="$2" data="$3"
  shift 3
  curl -sS -m 30 -o "$out" -w '%{http_code}' -X POST "$url" \
    -H 'Content-Type: application/json' "$@" -d "$data"
}

get_json() { # URL OUTFILE
  curl -sS -m 30 -o "$2" -w '%{http_code}' "$1"
}

# secret_in_file VALUE FILE — prints LEAK or clean. The value travels through
# the environment, never argv, so it cannot leak through ps.
secret_in_file() {
  SECRET_SCAN="$1" python3 - "$2" <<'PY'
import os, sys
secret = os.environ.get("SECRET_SCAN", "")
if not secret:
    print("clean")
    raise SystemExit(0)
data = open(sys.argv[1], "rb").read()
print("LEAK" if secret.encode() in data else "clean")
PY
}

assert_no_secret() { # LABEL SECRET FILE
  local label="$1" secret="$2" file="$3" verdict
  [ -f "$file" ] || return 0
  verdict="$(secret_in_file "$secret" "$file")"
  [ "$verdict" = "clean" ] || die "$label contains the API key literal (refusing to continue): $file"
  info "$label carries no API-key literal"
}

# --- runtime lifecycle -----------------------------------------------------

RT_PID=""
RT_LOG=""
PG_CTR="${KEEL_E2E_PG_CTR:-${KEEL_PG_CTR:-keel-pg}}"
PG_PORT="${KEEL_E2E_PG_PORT:-13741}"
PG_PASSWORD="${KEEL_E2E_PG_PASSWORD:-runtime}"
DB_CREATED=0
DB=""
DSN=""

stop_runtime() {
  if [ -z "$RT_PID" ]; then return 0; fi
  if kill -0 "$RT_PID" 2>/dev/null; then
    kill -TERM "$RT_PID" 2>/dev/null || true
    local i
    for i in $(seq 1 120); do
      kill -0 "$RT_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$RT_PID" 2>/dev/null || true
  fi
  wait "$RT_PID" 2>/dev/null || true
  RT_PID=""
}

# --- optional model-id rewriter (harness workaround) -----------------------
#
# internal/servicehost/services.go stores provider.ResolveProvider's second
# return value (the provider NAME, e.g. "openai") in the LLM service's model
# field, so the outbound chat request carries "model":"openai" instead of
# $KEEL_MODEL. This shim restores the configured id for the harness only; it
# forwards the request (and the real Authorization header) to the real gateway
# and streams the response back untouched. It is opt-in and its log records the
# rewrite so the wiring bug stays visible in the evidence.
PROXY_PID=""
PROXY_PORT=""
PROXY_LOG=""

stop_proxy() {
  if [ -z "$PROXY_PID" ]; then return 0; fi
  kill -TERM "$PROXY_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  PROXY_PID=""
}

start_rewrite_proxy() { # PORT LOG UPSTREAM TARGET_MODEL
  PROXY_PORT="$1"
  PROXY_LOG="$2"
  : > "$PROXY_LOG"
  python3 - "$PROXY_PORT" "$PROXY_LOG" "$3" "$4" <<'PY' &
import http.server, json, sys, urllib.error, urllib.request

port = int(sys.argv[1])
logpath = sys.argv[2]
upstream = sys.argv[3].rstrip("/")
target = sys.argv[4]


def log(line):
    with open(logpath, "a", encoding="utf-8") as fh:
        fh.write(line + "\n")


def upstream_url(path):
    # The driver appends /chat/completions to its base; the upstream base may
    # already carry /v1, so splice only the part the base does not own.
    if upstream.endswith("/v1") and path.startswith("/v1/"):
        return upstream + path[len("/v1"):]
    return upstream + path


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
            original = doc.get("model")
            doc["model"] = target
            body = json.dumps(doc).encode("utf-8")
        except Exception as exc:  # noqa: BLE001 - relay as a client error
            self.send_error(400, "rewriter: bad request body: %s" % exc)
            return

        req = urllib.request.Request(upstream_url(self.path), data=body, method="POST")
        for key, value in self.headers.items():
            if key.lower() in ("content-length", "host", "connection", "accept-encoding", "transfer-encoding"):
                continue
            req.add_header(key, value)
        status = 0
        try:
            try:
                resp = urllib.request.urlopen(req, timeout=300)
                status = resp.status
            except urllib.error.HTTPError as exc:
                resp = exc
                status = exc.code
            self.send_response(status)
            ctype = resp.headers.get("Content-Type")
            if ctype:
                self.send_header("Content-Type", ctype)
            # Close-delimited streaming: SSE bytes are flushed as they arrive.
            self.send_header("Connection", "close")
            self.end_headers()
            while True:
                chunk = resp.read(4096)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
            resp.close()
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as exc:  # noqa: BLE001 - upstream failure is data
            log("POST %s upstream error: %s: %s" % (self.path, type(exc).__name__, exc))
            try:
                self.send_error(502, "rewriter: upstream failure")
            except Exception:
                pass
        finally:
            log("POST %s model %r -> %r status=%s" % (self.path, original, target, status))

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
  PROXY_PID=$!
  local i
  for i in $(seq 1 60); do
    if [ "$(port_free "$PROXY_PORT")" = "used" ]; then
      info "model-id rewriter listening on 127.0.0.1:$PROXY_PORT -> $3 (model $4)"
      return 0
    fi
    kill -0 "$PROXY_PID" 2>/dev/null || die "model-id rewriter exited during startup"
    sleep 0.1
  done
  die "model-id rewriter did not start on port $PROXY_PORT"
}

cleanup() {
  local code=$?
  set +e
  stop_runtime
  stop_proxy
  if [ "$DB_CREATED" = "1" ] && [ "${KEEL_E2E_KEEP_DB:-0}" != "1" ]; then
    docker exec "$PG_CTR" psql -U postgres -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null 2>&1
    [ "${KEEL_E2E_KEEP_DB:-0}" = "1" ] || true
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

prepare_db() {
  if [ -n "${KEEL_E2E_DSN:-}" ]; then
    DSN="$KEEL_E2E_DSN"
    DB="$(DSN_ENV="$DSN" python3 -c 'import os,urllib.parse as u; print(u.urlparse(os.environ["DSN_ENV"]).path.lstrip("/").split("/")[0])')"
    DB_LOWER="$(printf '%s' "$DB" | tr '[:upper:]' '[:lower:]')"
    case "$DB_LOWER" in
      *e2e*|*test*|*smoke*|*accept*) ;;
      *) die "refusing KEEL_E2E_DSN database \"$DB\": the name must look dedicated (contains e2e/test/smoke/accept)" ;;
    esac
    info "using caller-provided dedicated DSN (database $DB); the script will not drop it"
    return 0
  fi

  command -v docker >/dev/null 2>&1 || blocked "docker is required for the isolated Postgres (or set KEEL_E2E_DSN)"
  local running
  running="$(docker inspect -f '{{.State.Running}}' "$PG_CTR" 2>/dev/null || true)"
  [ "$running" = "true" ] || blocked "dockerized Postgres $PG_CTR is not running; start it (docker compose up -d postgres) or set KEEL_E2E_DSN"

  DB="${KEEL_E2E_DB:-keel_real_model_e2e_$$}"
  DB_LOWER="$(printf '%s' "$DB" | tr '[:upper:]' '[:lower:]')"
  case "$DB_LOWER" in
    *e2e*|*test*|*smoke*|*accept*) ;;
    *) die "refusing to use database \"$DB\": the name must look dedicated (contains e2e/test/smoke/accept)" ;;
  esac
  DSN="postgres://postgres:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/${DB}?sslmode=disable"

  docker exec "$PG_CTR" psql -U postgres -v ON_ERROR_STOP=1 \
    -c "DROP DATABASE IF EXISTS \"$DB\" WITH (FORCE)" >/dev/null \
    || die "could not drop database $DB (is $PG_CTR reachable?)"
  docker exec "$PG_CTR" psql -U postgres -v ON_ERROR_STOP=1 \
    -c "CREATE DATABASE \"$DB\"" >/dev/null \
    || die "could not create database $DB"
  DB_CREATED=1
  local tables
  tables="$(docker exec "$PG_CTR" psql -U postgres -d "$DB" -tAc \
    "SELECT count(*) FROM pg_tables WHERE schemaname='public'" | tr -d '[:space:]')"
  [ "$tables" = "0" ] || die "database $DB is not fresh ($tables public tables)"
  info "fresh database $DB created in $PG_CTR"
}

ensure_binary() {
  local bin="${KEEL_E2E_BIN:-/tmp/keel-real-model-runtime}"
  if [ "${KEEL_E2E_SKIP_BUILD:-0}" = "1" ]; then
    [ -x "$bin" ] || blocked "KEEL_E2E_SKIP_BUILD=1 but $bin is not executable"
  else
    info "building runtime -> $bin"
    go build -o "$bin" ./cmd/runtime || die "go build ./cmd/runtime failed"
  fi
  printf '%s\n' "$bin"
}

start_runtime() { # PORT LOG EFFECTIVE_BASE_URL
  local port="$1" log="$2" effective_base="${3:-$BASE_URL}"
  RT_LOG="$log"
  : > "$log"
  info "starting runtime on 127.0.0.1:$port (log: $log)"
  env \
    KEEL_DSN="$DSN" \
    KEEL_ADMIN_KEY="$ADMIN_KEY" \
    KEEL_MODEL="$MODEL" \
    KEEL_BASE_URL="$effective_base" \
    KEEL_PROTOCOL="$PROTOCOL" \
    KEEL_API_KEY="$API_KEY" \
    KEEL_PROVIDER="$PROVIDER" \
    KEEL_SYSTEM_PROMPT="${KEEL_SYSTEM_PROMPT:-You are a deterministic integration-test agent. When the user asks you to call the echo tool, call it exactly once and then answer with the tool result.}" \
    "$BIN" -addr "127.0.0.1:$port" -cas "$EVIDENCE_DIR/cas" -streams 64 -runs 8 -admit-rate 200 \
    >>"$log" 2>&1 &
  RT_PID=$!

  local body pid i
  for i in $(seq 1 150); do
    body="$(curl -fsS -m 2 "http://127.0.0.1:$port/v1/health" 2>/dev/null || true)"
    if [ -n "$body" ]; then
      pid="$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid"))' 2>/dev/null || true)"
      [ "$pid" = "$RT_PID" ] || die "port $port answers with pid $pid, not $RT_PID (foreign process)"
      info "runtime healthy (pid $RT_PID)"
      return 0
    fi
    kill -0 "$RT_PID" 2>/dev/null || die "runtime exited during startup; see $log"
    sleep 0.2
  done
  die "runtime did not become healthy within 30s; see $log"
}

provision() { # PORT OUT_PREFIX -> sets TENANT SESSION
  local base="http://127.0.0.1:$1" prefix="$2" code
  TENANT="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  SESSION="$(uuidgen | tr '[:upper:]' '[:lower:]')"

  code="$(post_json "$base/v1/tenants" "$prefix-tenant.json" '{"name":"real-model-e2e"}' \
    -H "X-Tenant-ID: $TENANT" -H "Authorization: Bearer $ADMIN_KEY")"
  [ "$code" = "201" ] || die "create tenant returned HTTP $code: $(cat "$prefix-tenant.json" 2>/dev/null)"
  info "tenant created: $TENANT"

  code="$(post_json "$base/v1/sessions" "$prefix-session.json" "{\"sessionId\":\"$SESSION\"}" \
    -H "X-Tenant-ID: $TENANT")"
  [ "$code" = "201" ] || die "create session returned HTTP $code: $(cat "$prefix-session.json" 2>/dev/null)"
  info "session created: $SESSION"
}

submit_input() { # PORT OUT -> prints the committed seq
  local base="http://127.0.0.1:$1" out="$2" code
  code="$(post_json "$base/v1/sessions/$SESSION/input" "$out" \
    "$(printf '%s' "$PROMPT" | python3 -c 'import json,sys; print(json.dumps({"text": sys.stdin.read(), "requestId": sys.argv[1]}))' "$REQUEST_ID")" \
    -H "X-Tenant-ID: $TENANT")"
  [ "$code" = "201" ] || die "input returned HTTP $code: $(cat "$out" 2>/dev/null)"
  python3 - "$out" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
seq = doc.get("seq")
if not isinstance(seq, int) or seq <= 0:
    raise SystemExit("input response has no committed seq: %r" % doc)
print(seq)
PY
}

# --- assert / wait ---------------------------------------------------------

# wait_and_assert MODE BASE EVENTS_OUT SUMMARY_OUT REQ_SEQ EXPECT_TOOL MARKER
wait_and_assert() {
  local mode="$1" base="$2" events_out="$3" summary_out="$4" req_seq="$5" expect_tool="$6" marker="$7"

  BASE="$base" TENANT="$TENANT" SESSION="$SESSION" REQ_SEQ="$req_seq" \
  TIMEOUT_SEC="${KEEL_E2E_TIMEOUT_SEC:-180}" WAIT_MODE="$mode" \
  EXPECT_TOOL="$expect_tool" MARKER="$marker" SECRET_TO_SCAN="$API_KEY" \
  STRICT_MARKER="${KEEL_E2E_STRICT_MARKER:-0}" \
  MODEL="$MODEL" PROVIDER="$PROVIDER" REWRITE_MODEL="$REWRITE_MODEL" \
  ENDPOINT_HOST="$(endpoint_host)" \
  EVENTS_OUT="$events_out" SUMMARY_OUT="$summary_out" \
  python3 - <<'PY'
import json, os, re, time, urllib.error, urllib.request

base = os.environ["BASE"]
tenant = os.environ["TENANT"]
session = os.environ["SESSION"]
req_seq = int(os.environ["REQ_SEQ"])
timeout_s = float(os.environ["TIMEOUT_SEC"])
mode = os.environ["WAIT_MODE"]
expect_tool = os.environ.get("EXPECT_TOOL", "echo")
marker = os.environ.get("MARKER", "")
secret = os.environ.get("SECRET_TO_SCAN", "")
model = os.environ.get("MODEL", "")
provider = os.environ.get("PROVIDER", "")
rewrite_model = os.environ.get("REWRITE_MODEL", "")
endpoint_host = os.environ.get("ENDPOINT_HOST", "")
events_path = os.environ["EVENTS_OUT"]
summary_path = os.environ["SUMMARY_OUT"]


def get(path):
    req = urllib.request.Request(base + path, headers={"X-Tenant-ID": tenant})
    with urllib.request.urlopen(req, timeout=8) as resp:
        return json.loads(resp.read().decode())


def status():
    try:
        doc = get("/v1/sessions/%s/run" % session)
        return doc.get("status") or "", doc.get("lastError") or ""
    except urllib.error.HTTPError as exc:
        return "", "HTTP %d" % exc.code
    except Exception as exc:  # noqa: BLE001 - diagnostics only
        return "", "%s: %s" % (type(exc).__name__, exc)


def payload(ev):
    raw = ev.get("payload")
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except Exception:
            raw = {}
    return raw if isinstance(raw, dict) else {}


def text_of(p):
    out = []
    for block in p.get("content") or []:
        if isinstance(block, dict) and block.get("type") == "text":
            out.append(block.get("text") or "")
    return "".join(out)


def tool_calls_of(p):
    return [b for b in (p.get("content") or [])
            if isinstance(b, dict) and b.get("type") == "toolCall"]


def redact(text):
    text = text or ""
    if secret:
        text = text.replace(secret, "[REDACTED]")
    return text


# --- wait for the terminal fact, recording when each milestone first appears
events = []
milestones = {}
started = time.monotonic()
timed_out = False
last_progress = -1
last_status = ""
last_error = ""
final = None
error_msg = None
while True:
    try:
        events = get("/v1/sessions/%s/events?limit=500" % session).get("events") or []
    except Exception as exc:  # transient; keep the previous snapshot
        print("[wait] events fetch: %s: %s" % (type(exc).__name__, exc), flush=True)
    elapsed = time.monotonic() - started

    final = None
    error_msg = None
    tool_results = []
    calls = []
    for ev in events:
        kind = ev.get("kind")
        p = payload(ev)
        if kind == "user.message":
            milestones.setdefault("user", elapsed)
        elif kind == "assistant.message":
            stop = p.get("stopReason")
            calls.extend(tool_calls_of(p))
            if stop == "tool_use":
                milestones.setdefault("toolCall", elapsed)
            elif stop == "end_turn":
                final = p
                milestones.setdefault("final", elapsed)
            elif stop == "error":
                error_msg = p
                milestones.setdefault("error", elapsed)
        elif kind == "tool.result":
            tool_results.append(p)
            milestones.setdefault("toolResult", elapsed)

    if len(events) != last_progress:
        kinds = ",".join(ev.get("kind", "?") for ev in events)
        print("[wait] %6.2fs events=%d kinds=%s" % (elapsed, len(events), kinds), flush=True)
        last_progress = len(events)

    if mode == "failure":
        last_status, last_error = status()
        if error_msg is not None or last_status == "failed":
            break
    else:
        # Fail fast: a terminal error, or a direct answer that never called a
        # tool, cannot satisfy the round-trip assertions. Waiting the full
        # timeout would only delay the diagnosis.
        if error_msg is not None or (final is not None and (tool_results or not calls)):
            # Give the run row a moment to settle before the status assertion.
            for _ in range(20):
                last_status, last_error = status()
                if last_status in ("done", "failed"):
                    break
                time.sleep(0.1)
            break

    if elapsed >= timeout_s:
        timed_out = True
        last_status, last_error = status()
        break
    time.sleep(0.3)

elapsed = time.monotonic() - started
raw_events = json.dumps(events, ensure_ascii=False)
with open(events_path, "w", encoding="utf-8") as fh:
    fh.write(raw_events)

# --- assertions ------------------------------------------------------------
checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "ok": bool(ok), "detail": detail})
    if ok:
        print("PASS: %s" % name, flush=True)
    else:
        print("FAIL: %s %s" % (name, ("(%s)" % detail) if detail else ""), flush=True)


seqs = [ev.get("seq") for ev in events]
user_events = [ev for ev in events if ev.get("kind") == "user.message"]
assistants = [ev for ev in events if ev.get("kind") == "assistant.message"]
results = [ev for ev in events if ev.get("kind") == "tool.result"]
result_payloads = [payload(ev) for ev in results]
assistant_payloads = [payload(ev) for ev in assistants]
calls = []
for ev, p in zip(assistants, assistant_payloads):
    for call in tool_calls_of(p):
        calls.append((ev.get("seq"), call))

strict_marker = os.environ.get("STRICT_MARKER", "0") == "1"
marker_echoed = None
if mode == "success":
    marker_echoed = any(text_of(r) == marker for r in result_payloads) if marker else None

check("not timed out", not timed_out, "waited %.1fs" % elapsed)
check("events present", len(events) >= 2, "%d events" % len(events))
check("event seqs contiguous from 1",
      bool(seqs) and seqs == list(range(1, len(seqs) + 1)),
      "seqs=%s" % (seqs[:12],))

if mode == "success":
    check("exactly one user.message", len(user_events) == 1, "%d user events" % len(user_events))
    check("user.message seq matches the input receipt",
          bool(user_events) and user_events[0].get("seq") == req_seq,
          "user seq=%r receipt=%r" % (user_events[0].get("seq") if user_events else None, req_seq))
    check("at least one tool call", len(calls) >= 1, "%d tool calls" % len(calls))
    check("tool call is %r" % expect_tool,
          any(c.get("name") == expect_tool for _, c in calls),
          "names=%s" % [c.get("name") for _, c in calls])

    call_ids = [c.get("id") for _, c in calls]
    for call_id in call_ids:
        matching = [r for r in result_payloads if r.get("toolCallId") == call_id]
        check("exactly one tool.result for %s" % call_id, len(matching) == 1,
              "%d results" % len(matching))
        if matching:
            r = matching[0]
            check("tool.result %s is not an error" % call_id, not r.get("isError"),
                  "isError=%r" % r.get("isError"))
            check("tool.result %s carries text" % call_id, len(text_of(r)) > 0,
                  "textLen=%d" % len(text_of(r)))

    check("exactly one tool.result event", len(results) == 1, "%d tool results" % len(results))
    check("final answer present", final is not None, "no end_turn assistant message")
    if final is not None:
        check("final answer is end_turn", final.get("stopReason") == "end_turn",
              "stopReason=%r" % final.get("stopReason"))
        final_seq = None
        for ev, p in zip(assistants, assistant_payloads):
            if p is final:
                final_seq = ev.get("seq")
        last_result_seq = max([ev.get("seq") or 0 for ev in results], default=0)
        check("final answer comes after the tool result",
              final_seq is not None and final_seq > last_result_seq,
              "finalSeq=%r lastResultSeq=%r" % (final_seq, last_result_seq))
        check("final answer text is non-empty", len(text_of(final)) > 0,
              "textLen=%d" % len(text_of(final)))
    check("no terminal model error",
          not any(p.get("stopReason") == "error" for p in assistant_payloads),
          "errorMessage=%r" % redact(next((p.get("errorMessage") for p in assistant_payloads
                                           if p.get("stopReason") == "error"), ""))[:200])
    check("run status is done", last_status == "done", "status=%r lastError=%r" % (last_status, redact(last_error)))
    if strict_marker:
        check("tool result echoes the marker verbatim", bool(marker_echoed))
else:
    error_payloads = [p for p in assistant_payloads if p.get("stopReason") == "error"]
    check("failure is visible as an error event or a failed run",
          bool(error_payloads) or last_status == "failed",
          "status=%r" % last_status)
    messages = [redact(p.get("errorMessage") or "") for p in error_payloads] + [redact(last_error)]
    combined = " | ".join(messages)
    check("failure message names the upstream status",
          bool(re.search(r"(?i)(401|403|429|5\d\d|missing api key|unauthorized|invalid api key)", combined)),
          combined[:240])
    check("failure message is non-empty", bool(combined.strip()), combined[:240])

leaked = bool(secret) and secret in raw_events
check("API key literal absent from events", not leaked)

def ms(name):
    value = milestones.get(name)
    return int(value * 1000) if value is not None else None

summary = {
    "mode": mode,
    "ok": all(c["ok"] for c in checks),
    "model": model,
    "provider": provider,
    "endpoint": endpoint_host,
    "modelRewrite": rewrite_model,
    "durationMs": int(elapsed * 1000),
    "milestoneMs": {k: ms(k) for k in ("user", "toolCall", "toolResult", "final", "error")},
    "eventCount": len(events),
    "kinds": [ev.get("kind") for ev in events],
    "seqs": seqs,
    "userSeq": user_events[0].get("seq") if user_events else None,
    "requestSeq": req_seq,
    "toolCalls": [{"seq": seq, "id": c.get("id"), "name": c.get("name"),
                   "argsLen": len(json.dumps(c.get("arguments"), ensure_ascii=False))}
                  for seq, c in calls],
    "toolResults": [{"toolCallId": r.get("toolCallId"), "toolName": r.get("toolName"),
                     "isError": bool(r.get("isError")), "textLen": len(text_of(r))}
                    for r in result_payloads],
    "finalStopReason": (final or {}).get("stopReason"),
    "finalTextLen": len(text_of(final or {})),
    "markerEchoed": marker_echoed,
    "runStatus": last_status,
    "lastError": redact(last_error)[:400],
    "errorMessages": [m for m in (redact(p.get("errorMessage") or "")[:400] for p in
                      (assistant_payloads if mode == "success" else
                       [p for p in assistant_payloads if p.get("stopReason") == "error"])) if m],
    "timedOut": timed_out,
    "checks": checks,
}
with open(summary_path, "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)

failed = [c["check"] for c in checks if not c["ok"]]
if failed:
    print("summary: %s" % summary_path, flush=True)
    raise SystemExit(1)
raise SystemExit(0)
PY
}

print_summary() { # SUMMARY_JSON
  local summary="$1"
  [ -f "$summary" ] || return 0
  SUMMARY="$summary" python3 - <<'PY'
import json, os
doc = json.load(open(os.environ["SUMMARY"]))
d = doc.get("durationMs") or 0
m = doc.get("milestoneMs") or {}
print("  result      = %s" % ("PASS" if doc.get("ok") else "FAIL"))
print("  model       = %s (provider=%s)" % (doc.get("model"), doc.get("provider") or "auto"))
print("  endpoint    = %s" % (doc.get("endpoint") or "(default)"))
print("  elapsed     = %.2fs (user %sms, toolCall %sms, toolResult %sms, final %sms)" % (
    d / 1000.0, m.get("user"), m.get("toolCall"), m.get("toolResult"), m.get("final")))
print("  events      = %d seqs=%s" % (doc.get("eventCount"), doc.get("seqs")))
print("  kinds       = %s" % ",".join(doc.get("kinds") or []))
print("  toolCalls   = %s" % json.dumps(doc.get("toolCalls") or []))
print("  toolResults = %s" % json.dumps(doc.get("toolResults") or []))
print("  markerEchoed= %s  finalStopReason=%s finalTextLen=%s" % (
    doc.get("markerEchoed"), doc.get("finalStopReason"), doc.get("finalTextLen")))
print("  runStatus   = %s" % doc.get("runStatus"))
if doc.get("modelRewrite"):
    print("  workaround  = harness model-id rewriter -> %s" % doc.get("modelRewrite"))
if doc.get("errorMessages"):
    print("  errorMsgs   = %s" % json.dumps(doc.get("errorMessages")))
if doc.get("lastError"):
    print("  lastError   = %s" % doc.get("lastError"))
PY
}

# --- phases ----------------------------------------------------------------

run_success() {
  info "phase: success round trip"
  local port base prefix rewrite_port effective_base
  port="$(pick_port)"
  base="http://127.0.0.1:$port"
  prefix="$EVIDENCE_DIR/success"
  effective_base="$BASE_URL"

  if [ -n "$REWRITE_MODEL" ]; then
    rewrite_port="$(pick_port)"
    start_rewrite_proxy "$rewrite_port" "$EVIDENCE_DIR/rewrite-proxy.log" "$BASE_URL" "$REWRITE_MODEL"
    effective_base="http://127.0.0.1:$rewrite_port/v1"
  fi
  start_runtime "$port" "$EVIDENCE_DIR/runtime.log" "$effective_base"
  provision "$port" "$prefix"

  MARKER="${KEEL_E2E_MARKER:-keel-e2e-$(date +%s)-$$}"
  REQUEST_ID="real-model-$(date +%s)-$$"
  PROMPT="${KEEL_E2E_PROMPT:-Call the echo tool exactly once with the argument x set to \"$MARKER\". Then reply with one short sentence that contains the exact text the tool returned. Do not call any other tool.}"

  local req_seq t0 t1
  t0="$(now_ms)"
  req_seq="$(submit_input "$port" "$prefix-input.json")"
  info "input committed at seq $req_seq (requestId $REQUEST_ID)"

  wait_and_assert success "$base" "$EVIDENCE_DIR/events.json" "$EVIDENCE_DIR/summary.json" \
    "$req_seq" "${KEEL_E2E_EXPECT_TOOL:-echo}" "$MARKER" \
    || die "success round trip failed; see $EVIDENCE_DIR/summary.json and $EVIDENCE_DIR/runtime.log (if the error names an unknown model, see docs/REAL-MODEL-E2E.md §5: the servicehost model-id wiring bug; rerun with KEEL_E2E_MODEL_REWRITE=1)"
  t1="$(now_ms)"

  assert_no_secret "events.json" "$API_KEY" "$EVIDENCE_DIR/events.json"
  assert_no_secret "runtime.log" "$API_KEY" "$EVIDENCE_DIR/runtime.log"

  say ""
  say "PASS: real-model round trip"
  print_summary "$EVIDENCE_DIR/summary.json"
  say "  wallClock   = $((t1 - t0))ms"
  say "  evidence    = $EVIDENCE_DIR"
  stop_runtime
  stop_proxy
}

run_failure() {
  info "phase: failure path (invalid API key)"
  local port prefix req_seq rewrite_port effective_base
  port="$(pick_port)"
  prefix="$EVIDENCE_DIR/failure"
  effective_base="$BASE_URL"
  # A deliberately invalid value; never a real credential.
  API_KEY="invalid-key-local-e2e-$(date +%s)-$$"

  if [ -n "$REWRITE_MODEL" ]; then
    rewrite_port="$(pick_port)"
    start_rewrite_proxy "$rewrite_port" "$EVIDENCE_DIR/failure-rewrite-proxy.log" "$BASE_URL" "$REWRITE_MODEL"
    effective_base="http://127.0.0.1:$rewrite_port/v1"
  fi
  start_runtime "$port" "$EVIDENCE_DIR/failure-runtime.log" "$effective_base"
  provision "$port" "$prefix"
  PROMPT="${KEEL_E2E_FAILURE_PROMPT:-Call the echo tool with x=\"failure-path-probe\".}"
  REQUEST_ID="real-model-failure-$(date +%s)-$$"
  MARKER="failure-path-probe"
  req_seq="$(submit_input "$port" "$prefix-input.json")"
  info "input committed at seq $req_seq; waiting for the visible failure"

  wait_and_assert failure "http://127.0.0.1:$port" \
    "$EVIDENCE_DIR/failure-events.json" "$EVIDENCE_DIR/failure-summary.json" \
    "$req_seq" echo "$MARKER" \
    || die "failure path assertions failed; see $EVIDENCE_DIR/failure-summary.json and $EVIDENCE_DIR/failure-runtime.log"

  assert_no_secret "failure-events.json" "$API_KEY" "$EVIDENCE_DIR/failure-events.json"
  assert_no_secret "failure-runtime.log" "$API_KEY" "$EVIDENCE_DIR/failure-runtime.log"

  say ""
  say "PASS: failure path is visible and non-leaking"
  print_summary "$EVIDENCE_DIR/failure-summary.json"
  say "  evidence    = $EVIDENCE_DIR/failure-summary.json"
  stop_runtime
  stop_proxy
}

# --- main ------------------------------------------------------------------

resolve_credentials
print_config

if [ "$MODE" = "check" ]; then
  say "check: configuration resolves; no network or database was touched"
  exit 0
fi

EVIDENCE_DIR="${KEEL_E2E_EVIDENCE_DIR:-$(mktemp -d /tmp/keel-real-model.XXXXXX)}"
mkdir -p "$EVIDENCE_DIR"
EVIDENCE_DIR="$(cd "$EVIDENCE_DIR" && pwd)"
ADMIN_KEY="local-admin-e2e-$(date +%s)-$$"

info "evidence dir: $EVIDENCE_DIR"
BIN="$(ensure_binary)"
prepare_db

case "$MODE" in
  success) run_success ;;
  failure) run_failure ;;
  both)    run_success; run_failure ;;
  *) die "internal: unknown mode $MODE" ;;
esac

say ""
say "evidence: $EVIDENCE_DIR (raw logs/events; not committed)"
