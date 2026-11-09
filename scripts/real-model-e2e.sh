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
# --models is the provider-configuration phase: it boots `keel serve` with a
# tenant models document (KEEL_MODELS_CONFIG / --models-config) whose provider
# points at a RECORDING proxy in front of the real gateway, then drives two
# inputs through the product API with different per-input selections and
# asserts the wire request itself: model id, reasoning_effort, max_tokens,
# credential presence, plus GET /v1/providers and GET /v1/models, the persisted
# selection fact and the run.finished identity. This is the gate for "the
# configuration reached the wire", not just "a run finished".
#
# Usage:
#   scripts/real-model-e2e.sh                 # success round trip
#   scripts/real-model-e2e.sh --with-failure  # success + 401 failure path
#   scripts/real-model-e2e.sh --failure-auth  # 401 failure path only
#   scripts/real-model-e2e.sh --models        # provider-configuration phase (after)
#   scripts/real-model-e2e.sh --models-baseline  # the pre-change binary (before)
#   scripts/real-model-e2e.sh --models-legacy    # this binary on the flat flags (compat)
#   scripts/real-model-e2e.sh --settings         # settings write -> hot reload -> reset
#   scripts/real-model-e2e.sh --models-all    # all three phases in one evidence directory
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
    --models) MODE=models ;;
    --models-baseline) MODE=models-baseline ;;
    --models-legacy) MODE=models-legacy ;;
    --models-all) MODE=models-all ;;
    --settings) MODE=settings ;;
    -h|--help)
      sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'
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

# --- recording proxy (provider-config phase) -------------------------------
#
# The provider-config phase configures the tenant's provider base URL to point
# here, so the ONLY way the loop can reach the model is through this proxy. It
# forwards to the real gateway (streaming SSE back untouched) and records what
# the wire actually carried: the request body plus whether a credential was
# present and matched. The credential VALUE is never written: only the boolean
# verdict, computed inside the proxy.
REC_PID=""
REC_PORT=""
REC_BODIES=""

stop_record_proxy() {
  if [ -z "$REC_PID" ]; then return 0; fi
  kill -TERM "$REC_PID" 2>/dev/null || true
  wait "$REC_PID" 2>/dev/null || true
  REC_PID=""
}

start_recording_proxy() { # PORT LOG BODIES_JSONL UPSTREAM
  REC_PORT="$1"
  REC_BODIES="$3"
  : > "$2"
  : > "$3"
  python3 - "$1" "$2" "$3" "$4" <<'PY' &
import http.server, json, os, sys, urllib.error, urllib.request

port = int(sys.argv[1])
logpath = sys.argv[2]
bodies = sys.argv[3]
upstream = sys.argv[4].rstrip("/")
expect_key = os.environ.get("PROXY_EXPECT_KEY", "")


def log(line):
    with open(logpath, "a", encoding="utf-8") as fh:
        fh.write(line + "\n")


def record(doc):
    with open(bodies, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")


def upstream_url(path):
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
        except Exception as exc:  # noqa: BLE001 - a non-JSON body is a fact too
            doc = {}
            log("POST %s unparseable body: %s" % (self.path, exc))
        auth = self.headers.get("Authorization") or ""
        scheme = auth.split(" ", 1)[0] if auth else ""
        token = auth.split(" ", 1)[1] if " " in auth else ""
        record({
            "path": self.path,
            "model": doc.get("model"),
            "reasoningEffort": doc.get("reasoning_effort"),
            "maxTokens": doc.get("max_tokens"),
            "maxOutputTokens": doc.get("max_output_tokens"),
            "stream": doc.get("stream"),
            "hasTools": bool(doc.get("tools")),
            "topLevelKeys": sorted(doc.keys()),
            "authScheme": scheme,
            "authPresent": bool(auth),
            "authMatchesExpected": bool(expect_key) and token == expect_key,
        })

        req = urllib.request.Request(upstream_url(self.path), data=raw, method="POST")
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
                self.send_error(502, "recorder: upstream failure")
            except Exception:
                pass
        finally:
            log("POST %s model=%r status=%s" % (self.path, doc.get("model"), status))

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
  REC_PID=$!
  local i
  for i in $(seq 1 60); do
    if [ "$(port_free "$REC_PORT")" = "used" ]; then
      info "recording proxy on 127.0.0.1:$REC_PORT -> $4 (bodies: $3)"
      return 0
    fi
    kill -0 "$REC_PID" 2>/dev/null || die "recording proxy exited during startup"
    sleep 0.1
  done
  die "recording proxy did not start on port $REC_PORT"
}

# write_models_config FILE PROXY_BASE MODEL — the tenant's models document. It
# configures exactly what the phase asserts: one visible provider pointing at
# the recording proxy, one hidden provider that must never be listed, a model
# rule pinning maxOutputTokens (so the value on the wire is provably the
# configured one, not a default) and the reasoning-level wire mapping.
write_models_config() {
  local file="$1" base="$2" model="$3"
  MODELS_FILE="$file" PROXY_BASE="$base" MODEL_ID="$model" python3 - <<'PY'
import json, os

doc = {
    "schemaVersion": 1,
    "providers": [
        {
            "id": "s2api-e2e",
            "name": "S2API (recording proxy)",
            "api": {"type": "openai-chat-completions", "baseUrl": os.environ["PROXY_BASE"]},
            "access": {"type": "api-key", "env": "KEEL_E2E_MODEL_KEY"},
            "modelIds": [os.environ["MODEL_ID"]],
            "visibility": "visible",
        },
        {
            "id": "hidden-e2e",
            "name": "Hidden provider (must not be listed)",
            "api": {"type": "openai-chat-completions", "baseUrl": os.environ["PROXY_BASE"]},
            "access": {"type": "api-key", "env": "KEEL_E2E_MODEL_KEY"},
            "modelIds": [os.environ["MODEL_ID"] + "-hidden"],
            "visibility": "hidden",
        },
    ],
    "modelRules": [
        {
            "match": os.environ["MODEL_ID"],
            "config": {
                "properties": {"contextWindow": 64000},
                "options": {
                    "reasoningLevel": {
                        "values": ["off", "low", "high"],
                        "map": {
                            "off": {},
                            "low": {"reasoning_effort": "low"},
                            "high": {"reasoning_effort": "high"},
                        },
                    },
                    "maxOutputTokens": {"max": 1024, "map": {"": {}}},
                },
            },
        }
    ],
    "selection": {
        "providerId": "s2api-e2e",
        "modelId": os.environ["MODEL_ID"],
        "reasoningLevel": "low",
    },
}
with open(os.environ["MODELS_FILE"], "w", encoding="utf-8") as fh:
    json.dump(doc, fh, ensure_ascii=False, indent=2)
PY
  info "models document written: $file (model $model via the recording proxy)"
}

# --- keel serve lifecycle (provider-config phase) --------------------------

SERVE_PID=""
SERVE_LOG=""
JAR=""

stop_serve() {
  if [ -z "$SERVE_PID" ]; then return 0; fi
  if kill -0 "$SERVE_PID" 2>/dev/null; then
    kill -TERM "$SERVE_PID" 2>/dev/null || true
    local i
    for i in $(seq 1 120); do
      kill -0 "$SERVE_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$SERVE_PID" 2>/dev/null || true
  fi
  wait "$SERVE_PID" 2>/dev/null || true
  SERVE_PID=""
}

start_serve() { # PORT LOG MODELS_FILE PASSWORD TENANT
  local port="$1" log="$2" models="$3" password="$4" tenant="$5"
  SERVE_LOG="$log"
  : > "$log"
  info "starting keel serve on 127.0.0.1:$port (log: $log)"
  # KEEL_E2E_MODEL_KEY is the variable the models document's access.env names:
  # the credential reaches the process through the environment, never through
  # the document. It is deliberately NOT the flat KEEL_API_KEY, so the picker's
  # credential-aware registry stays observable (the keyless builtin providers
  # must not be offered).
  env \
    KEEL_DSN="$DSN" \
    KEEL_SECRET_KEY="${KEEL_E2E_SECRET_KEY:-$(openssl rand -hex 32)}" \
    KEEL_MODELS_CONFIG="$models" \
    KEEL_SYSTEM_PROMPT="${KEEL_SYSTEM_PROMPT:-You are a deterministic integration-test agent. When the user asks you to call the echo tool, call it exactly once and then answer with the tool result.}" \
    "$SERVE_BIN" serve -addr "127.0.0.1:$port" -cas "$EVIDENCE_DIR/serve-cas" -password "$password" -tenant "$tenant" \
    >>"$log" 2>&1 &
  SERVE_PID=$!

  local body pid i
  for i in $(seq 1 150); do
    body="$(curl -fsS -m 2 "http://127.0.0.1:$port/v1/health" 2>/dev/null || true)"
    if [ -n "$body" ]; then
      pid="$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid"))' 2>/dev/null || true)"
      [ "$pid" = "$SERVE_PID" ] || die "port $port answers with pid $pid, not $SERVE_PID (foreign process)"
      info "keel serve healthy (pid $SERVE_PID)"
      return 0
    fi
    kill -0 "$SERVE_PID" 2>/dev/null || die "keel serve exited during startup; see $log"
    sleep 0.2
  done
  die "keel serve did not become healthy within 30s; see $log"
}

serve_login() { # PORT JAR PASSWORD
  local code
  code="$(curl -sS -m 10 -o "$EVIDENCE_DIR/serve-login.json" -w '%{http_code}' \
    -c "$2" -X POST "http://127.0.0.1:$1/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "$(PASSWORD_ENV="$3" python3 -c 'import json,os; print(json.dumps({"password": os.environ["PASSWORD_ENV"]}))')")"
  [ "$code" = "200" ] || die "login returned HTTP $code: $(cat "$EVIDENCE_DIR/serve-login.json" 2>/dev/null)"
  info "logged in (cookie jar: $2)"
}

# serve_json METHOD JAR URL OUT [DATA] -> prints the HTTP status
serve_json() {
  local method="$1" jar="$2" url="$3" out="$4" data="${5:-}"
  if [ -n "$data" ]; then
    curl -sS -m 30 -o "$out" -w '%{http_code}' -b "$jar" -c "$jar" -X "$method" "$url" \
      -H 'Content-Type: application/json' -d "$data"
  else
    curl -sS -m 30 -o "$out" -w '%{http_code}' -b "$jar" -c "$jar" -X "$method" "$url"
  fi
}

start_rewrite_proxy() { # PORT LOG UPSTREAM TARGET_MODEL  PROXY_PORT="$1"
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
  stop_record_proxy
  stop_serve
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

ensure_serve_binary() {
  local bin="${KEEL_E2E_SERVE_BIN:-/tmp/keel-real-model-serve}"
  if [ "${KEEL_E2E_SKIP_BUILD:-0}" = "1" ]; then
    [ -x "$bin" ] || blocked "KEEL_E2E_SKIP_BUILD=1 but $bin is not executable"
  else
    info "building keel serve -> $bin"
    go build -o "$bin" ./cmd/keel || die "go build ./cmd/keel failed"
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

# --- provider-config phase -------------------------------------------------
#
# The gate for "the tenant's provider configuration reaches the wire". The
# provider's base URL points at the recording proxy, so a request that did not
# carry the configured model/protocol/options is a visible failure rather than
# a silent one. Two inputs with different per-input selections prove the
# selection fact travels from the client through the log to the attempt.
run_provider_config() {
  info "phase: provider configuration (keel serve + models document + recording proxy)"
  local port rec_port serve_port tenant password models bodies prefix
  port="$(pick_port)"
  rec_port="$(pick_port)"
  serve_port="$(pick_port)"
  prefix="$EVIDENCE_DIR/models"
  bodies="$EVIDENCE_DIR/wire-requests.jsonl"
  models="$EVIDENCE_DIR/models-config.json"
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  password="${KEEL_E2E_SERVE_PASSWORD:-keel-e2e-$$-$(date +%s)}"
  JAR="$EVIDENCE_DIR/cookies.txt"
  : > "$JAR"

  SERVE_BIN="$(ensure_serve_binary)"
  # The credential variable the models document names is exported here, so the
  # harness and the server agree on which providers this process can resolve —
  # which is what the picker assertion below checks.
  export KEEL_E2E_MODEL_KEY="$API_KEY"
  PROXY_EXPECT_KEY="$API_KEY" start_recording_proxy "$rec_port" "$EVIDENCE_DIR/recording-proxy.log" "$bodies" "$BASE_URL"
  write_models_config "$models" "http://127.0.0.1:$rec_port/v1" "$MODEL"
  start_serve "$serve_port" "$EVIDENCE_DIR/serve.log" "$models" "$password" "$tenant"

  local base="http://127.0.0.1:$serve_port" code
  serve_login "$serve_port" "$JAR" "$password"

  # The capability surface: an authenticated client must see the tenant's
  # visible provider and never the hidden one.
  code="$(serve_json GET "$JAR" "$base/v1/providers" "$EVIDENCE_DIR/providers.json")"
  [ "$code" = "200" ] || die "GET /v1/providers returned HTTP $code: $(cat "$EVIDENCE_DIR/providers.json" 2>/dev/null)"
  code="$(serve_json GET "$JAR" "$base/v1/models" "$EVIDENCE_DIR/models.json")"
  [ "$code" = "200" ] || die "GET /v1/models returned HTTP $code: $(cat "$EVIDENCE_DIR/models.json" 2>/dev/null)"

  code="$(serve_json POST "$JAR" "$base/v1/sessions" "$EVIDENCE_DIR/session.json" '{}')"
  [ "$code" = "201" ] || die "create session returned HTTP $code: $(cat "$EVIDENCE_DIR/session.json" 2>/dev/null)"
  SESSION="$(SESSION_FILE="$EVIDENCE_DIR/session.json" python3 -c 'import json,os; d=json.load(open(os.environ["SESSION_FILE"])); print(d.get("sessionId") or d.get("id") or "")')"
  [ -n "$SESSION" ] || die "session response carries no id: $(cat "$EVIDENCE_DIR/session.json")"
  info "serve session created: $SESSION"

  # Refusals, on a real session: the hidden provider's model, and a selection
  # carrying a key the vocabulary does not define. Both must be 400 AND the body
  # must name the refusal (a session-gate "session not found" would be a pass for
  # the wrong reason).
  local before_events
  before_events="$(serve_json GET "$JAR" "$base/v1/sessions/$SESSION/events?limit=500" "$EVIDENCE_DIR/refused-before.json" >/dev/null; EVENTS_FILE="$EVIDENCE_DIR/refused-before.json" python3 -c 'import json,os; print(len((json.load(open(os.environ["EVENTS_FILE"])).get("events") or [])))')"
  code="$(serve_json POST "$JAR" "$base/v1/sessions/$SESSION/input" "$EVIDENCE_DIR/refused-hidden.json" \
    "$(MODEL_ID="$MODEL" python3 -c 'import json,os; print(json.dumps({"text":"x","requestId":"hidden-probe","selection":{"providerId":"hidden-e2e","modelId":os.environ["MODEL_ID"]+"-hidden"}}))')")"
  HIDDEN_REFUSED_CODE="$code"
  info "selecting a hidden provider's model -> HTTP $code (expected 400)"
  code="$(serve_json POST "$JAR" "$base/v1/sessions/$SESSION/input" "$EVIDENCE_DIR/refused-unknown-key.json" \
    "$(MODEL_ID="$MODEL" python3 -c 'import json,os; print(json.dumps({"text":"x","requestId":"unknown-key-probe","selection":{"providerId":"s2api-e2e","modelId":os.environ["MODEL_ID"],"apiKey":"sk-probe-not-a-credential"}}))')")"
  UNKNOWN_KEY_REFUSED_CODE="$code"
  info "a selection carrying an unknown key -> HTTP $code (expected 400)"
  serve_json GET "$JAR" "$base/v1/sessions/$SESSION/events?limit=500" "$EVIDENCE_DIR/refused-after.json" >/dev/null
  REFUSED_EVENTS_BEFORE="$before_events"

  local level attempt request_id prompt
  for level in low high; do
    prompt="Call the echo tool exactly once with the argument x set to \"$level-marker\". Then reply with one short sentence that contains the exact text the tool returned."
    for attempt in 1 2 3; do
      request_id="models-$level-$attempt-$(date +%s)-$$"
      code="$(serve_json POST "$JAR" "$base/v1/sessions/$SESSION/input" "$prefix-input-$level.json" \
        "$(LEVEL="$level" REQUEST_ID="$request_id" PROMPT="$prompt" MODEL_ID="$MODEL" python3 -c '
import json, os
print(json.dumps({
    "text": os.environ["PROMPT"],
    "requestId": os.environ["REQUEST_ID"],
    "delivery": "queue",
    "selection": {"providerId": "s2api-e2e", "modelId": os.environ["MODEL_ID"], "reasoningLevel": os.environ["LEVEL"]},
}))'  )")"
      [ "$code" = "201" ] || die "input ($level, attempt $attempt) returned HTTP $code: $(cat "$prefix-input-$level.json" 2>/dev/null)"
      info "input committed with reasoningLevel=$level (attempt $attempt)"
      wait_serve_run "$base" "$JAR" "$SESSION" "$EVIDENCE_DIR/events-$level.json" "$EVIDENCE_DIR/run-$level.json" \
        || die "the $level attempt did not settle; see $EVIDENCE_DIR/serve.log"
      if RUN_FILE="$EVIDENCE_DIR/run-$level.json" python3 -c 'import json,os,sys; sys.exit(0 if json.load(open(os.environ["RUN_FILE"])).get("status") == "done" else 1)'; then
        break
      fi
      # A real gateway can answer 5xx transiently; the retry is the deployment's
      # own behaviour (the kernel re-queues a failed run), so the harness submits
      # a fresh input instead of failing the gate on provider weather.
      info "attempt $attempt for $level settled $(RUN_FILE="$EVIDENCE_DIR/run-$level.json" python3 -c 'import json,os; print(json.load(open(os.environ["RUN_FILE"])).get("status"))'); resubmitting"
      [ "$attempt" != "3" ] || die "the $level attempt kept failing upstream; see $EVIDENCE_DIR/run-$level.json"
    done
  done

  MODEL_ID="$MODEL" BODIES="$bodies" PROVIDERS="$EVIDENCE_DIR/providers.json" \
  MODELS="$EVIDENCE_DIR/models.json" EVENTS_LOW="$EVIDENCE_DIR/events-low.json" \
  HIDDEN_REFUSED_CODE="$HIDDEN_REFUSED_CODE" UNKNOWN_KEY_REFUSED_CODE="$UNKNOWN_KEY_REFUSED_CODE" \
  HIDDEN_REFUSED_BODY="$(cat "$EVIDENCE_DIR/refused-hidden.json" 2>/dev/null)" \
  UNKNOWN_KEY_REFUSED_BODY="$(cat "$EVIDENCE_DIR/refused-unknown-key.json" 2>/dev/null)" \
  REFUSED_EVENTS_BEFORE="$REFUSED_EVENTS_BEFORE" \
  REFUSED_EVENTS_AFTER="$(EVENTS_FILE="$EVIDENCE_DIR/refused-after.json" python3 -c 'import json,os; print(len((json.load(open(os.environ["EVENTS_FILE"])).get("events") or [])))' 2>/dev/null || echo "")" \
  EVENTS_HIGH="$EVIDENCE_DIR/events-high.json" INPUT_LOW="$prefix-input-low.json" \
  INPUT_HIGH="$prefix-input-high.json" SECRET_TO_SCAN="$API_KEY" SESSION="$SESSION" \
  SERVE_LOG="$EVIDENCE_DIR/serve.log" RECORD_LOG="$EVIDENCE_DIR/recording-proxy.log" \
  SUMMARY_OUT="$EVIDENCE_DIR/provider-config-summary.json" \
  python3 - <<'PY' || die "provider-config assertions failed; see $EVIDENCE_DIR/provider-config-summary.json"
import json, os, re

model = os.environ["MODEL_ID"]
secret = os.environ["SECRET_TO_SCAN"]


def load(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


bodies = [json.loads(line) for line in open(os.environ["BODIES"], encoding="utf-8") if line.strip()]
providers = load(os.environ["PROVIDERS"])
models = load(os.environ["MODELS"])
checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "ok": bool(ok), "detail": detail})
    print(("PASS: " if ok else "FAIL: ") + name + ((" (%s)" % detail) if detail else ""), flush=True)


def payload(ev):
    raw = ev.get("payload")
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except Exception:
            raw = {}
    return raw if isinstance(raw, dict) else {}


def text_of(p):
    return "".join(b.get("text") or "" for b in (p.get("content") or [])
                   if isinstance(b, dict) and b.get("type") == "text")


check("the model was actually called through the configured provider (>=2 requests)",
      len(bodies) >= 2, "%d requests" % len(bodies))
chat = [b for b in bodies if b.get("path", "").endswith("/chat/completions")]
check("every request used the configured wire protocol (chat completions)",
      bool(chat) and len(chat) == len(bodies), "paths=%s" % [b.get("path") for b in bodies])

# Select by reasoning effort: a retried attempt must not be mistaken for the
# other level.
low = next((b for b in chat if b.get("reasoningEffort") == "low"), {})
high = next((b for b in chat if b.get("reasoningEffort") == "high"), {})

check("wire model id is the configured modelId",
      bool(chat) and all(b.get("model") == model for b in chat),
      "models=%s" % sorted({b.get("model") for b in chat}))
check("selection low reached the wire", low.get("reasoningEffort") == "low",
      "reasoningEffort=%r" % low.get("reasoningEffort"))
check("selection high reached the wire", high.get("reasoningEffort") == "high",
      "reasoningEffort=%r" % high.get("reasoningEffort"))
check("configured maxOutputTokens reached the wire",
      bool(chat) and all(b.get("maxTokens") == 1024 for b in chat),
      "maxTokens=%s" % [b.get("maxTokens") for b in chat])
check("credential was present and matched the configured value on every request",
      bool(chat) and all(b.get("authPresent") and b.get("authMatchesExpected") for b in chat),
      "auth=%s" % [(b.get("authScheme"), b.get("authMatchesExpected")) for b in chat])
check("requests are streaming and carry the tool definitions",
      bool(chat) and all(b.get("stream") and b.get("hasTools") for b in chat),
      "stream=%s tools=%s" % ([b.get("stream") for b in chat], [b.get("hasTools") for b in chat]))

provider_ids = [p.get("providerId") for p in providers.get("providers") or []]
check("GET /v1/providers lists the tenant provider", "s2api-e2e" in provider_ids, "providers=%s" % provider_ids)
check("GET /v1/providers hides the hidden provider", "hidden-e2e" not in provider_ids, "providers=%s" % provider_ids)
check("the settings view still publishes the keyless builtin providers (issues, not silence)",
      len(provider_ids) > 1, "providers=%d" % len(provider_ids))
visible_models = [m.get("modelId") for m in models.get("models") or []]
check("GET /v1/models lists the selected model", model in visible_models, "models=%s" % visible_models)
check("GET /v1/models does not leak the hidden model",
      not any("hidden" in (m or "") for m in visible_models), "models=%s" % visible_models)
# The picker list is the credential-aware registry: this process holds only the
# tenant's key, so the keyless builtin providers must not be offered.
# The picker list is the credential-aware registry: only models whose provider
# credential this process can actually resolve are offered. The settings view
# above keeps everything visible with issues.
resolvable = {p.get("providerId") for p in providers.get("providers") or []
              if (p.get("access") or {}).get("env") and os.environ.get((p.get("access") or {})["env"])}
rows = [(m.get("providerId"), m.get("modelId")) for m in models.get("models") or []]
check("GET /v1/models offers only models of credential-resolvable providers",
      bool(rows) and all(pid in resolvable for pid, _ in rows),
      "rows=%s resolvable=%s" % (rows, sorted(resolvable)))
check("keyless providers are absent from the picker",
      not any(pid in {"openai", "ollama", "anthropic", "openrouter", "deepseek"} for pid, _ in rows),
      "rows=%s" % rows)
check("the refusal probes did not write anything to the log",
      os.environ["REFUSED_EVENTS_BEFORE"] == os.environ["REFUSED_EVENTS_AFTER"],
      "before=%s after=%s" % (os.environ["REFUSED_EVENTS_BEFORE"], os.environ["REFUSED_EVENTS_AFTER"]))
check("the hidden-provider refusal names the refusal (not the session gate)",
      "not found" not in os.environ["HIDDEN_REFUSED_BODY"].lower(),
      os.environ["HIDDEN_REFUSED_BODY"][:160])
check("the unknown-key refusal names the refusal (not the session gate)",
      "not found" not in os.environ["UNKNOWN_KEY_REFUSED_BODY"].lower(),
      os.environ["UNKNOWN_KEY_REFUSED_BODY"][:160])
check("selecting a hidden provider's model is refused", os.environ["HIDDEN_REFUSED_CODE"] == "400",
      "HTTP %s" % os.environ["HIDDEN_REFUSED_CODE"])
check("a selection carrying an unknown key is refused", os.environ["UNKNOWN_KEY_REFUSED_CODE"] == "400",
      "HTTP %s" % os.environ["UNKNOWN_KEY_REFUSED_CODE"])
surface = json.dumps({"providers": providers, "models": models})
check("capability surface carries no credential-shaped value", not re.search(r"sk-[A-Za-z0-9]", surface))

for label, level in (("low", "low"), ("high", "high")):
    events = load(os.environ["EVENTS_%s" % label.upper()]).get("events") or []
    users = [payload(ev) for ev in events if ev.get("kind") == "user.message"]
    assistants = [payload(ev) for ev in events if ev.get("kind") == "assistant.message"]
    finished = [payload(ev) for ev in events if ev.get("kind") == "run.finished"]
    expected = {"providerId": "s2api-e2e", "modelId": model, "reasoningLevel": level}
    check("%s: the input carries the selection fact verbatim" % label,
          any((u.get("selection") or {}) == expected for u in users),
          "selections=%s" % [u.get("selection") for u in users])
    check("%s: an assistant answered" % label, any(text_of(a) for a in assistants))
    check("%s: run.finished records the execution identity" % label,
          any(f.get("model") == model and f.get("provider") == "s2api-e2e" for f in finished),
          "identities=%s" % [(f.get("provider"), f.get("model")) for f in finished])
    check("%s: no terminal model error" % label,
          not any(a.get("stopReason") == "error" for a in assistants))

for label, path in (("events low", os.environ["EVENTS_LOW"]), ("events high", os.environ["EVENTS_HIGH"]),
                    ("serve log", os.environ["SERVE_LOG"]), ("recording proxy log", os.environ["RECORD_LOG"]),
                    ("input low", os.environ["INPUT_LOW"]), ("input high", os.environ["INPUT_HIGH"]),
                    ("wire requests", os.environ["BODIES"])):
    data = open(path, encoding="utf-8").read()
    check("%s: no API-key literal" % label, not (secret and secret in data))

summary = {"ok": all(c["ok"] for c in checks), "model": model,
           "session": os.environ.get("SESSION", ""), "requests": bodies, "checks": checks}
with open(os.environ["SUMMARY_OUT"], "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
print("summary: %s" % os.environ["SUMMARY_OUT"], flush=True)
raise SystemExit(0 if all(c["ok"] for c in checks) else 1)
PY

  stop_serve
  stop_record_proxy
  say ""
  say "PASS: provider configuration reached the wire"
  say "  evidence   = $EVIDENCE_DIR/wire-requests.jsonl, providers.json, models.json, events-{low,high}.json"
}

# wait_serve_run BASE JAR SESSION EVENTS_OUT RUN_OUT — poll until the session's
# run settles done, then fetch the events.
wait_serve_run() { # BASE JAR SESSION EVENTS_OUT RUN_OUT [EXPECTED_STATUS]
  local base="$1" jar="$2" session="$3" events_out="$4" run_out="$5" expected="${6:-done}" i status assist
  for i in $(seq 1 $(( ${KEEL_E2E_TIMEOUT_SEC:-180} * 2 ))); do
    serve_json GET "$jar" "$base/v1/sessions/$session/run" "$run_out" >/dev/null 2>&1 || true
    status="$(RUN_FILE="$run_out" python3 -c 'import json,os
try:
    print(json.load(open(os.environ["RUN_FILE"])).get("status") or "")
except Exception:
    print("")' 2>/dev/null || true)"
    case "$status" in
      done|failed|cancelled)
        serve_json GET "$jar" "$base/v1/sessions/$session/events?limit=500" "$events_out" >/dev/null 2>&1 || true
        assist="$(EVENTS_FILE="$events_out" python3 -c 'import json,os
try:
    ev = (json.load(open(os.environ["EVENTS_FILE"])).get("events") or [])
except Exception:
    print(0); raise SystemExit
print(sum(1 for e in ev if e.get("kind") in ("assistant.message", "run.finished")))' 2>/dev/null || true)"
        if [ "$status" = "$expected" ] && [ "${assist:-0}" -ge 1 ]; then
          return 0
        fi
        ;;
    esac
    sleep 0.5
  done
  return 1
}

# --- model settings phase (write -> hot reload -> reset) -------------------
#
# The acceptance gate for keel-model-settings: a settings write must change what
# the NEXT run uses, without a restart, and a bad document must be refused
# without disturbing the configuration that is serving.
run_model_settings() {
  info "phase: model settings (write -> hot reload -> reset)"
  local rec_port serve_port tenant password models bodies prefix
  rec_port="$(pick_port)"
  serve_port="$(pick_port)"
  prefix="$EVIDENCE_DIR/settings"
  bodies="$EVIDENCE_DIR/settings-wire.jsonl"
  models="$EVIDENCE_DIR/settings-boot-models.json"
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  password="${KEEL_E2E_SERVE_PASSWORD:-keel-e2e-$$-$(date +%s)}"
  JAR="$EVIDENCE_DIR/settings-cookies.txt"
  : > "$JAR"

  SERVE_BIN="$(ensure_serve_binary)"
  export KEEL_E2E_MODEL_KEY="$API_KEY"
  PROXY_EXPECT_KEY="$API_KEY" start_recording_proxy "$rec_port" "$EVIDENCE_DIR/settings-recording-proxy.log" "$bodies" "$BASE_URL"
  # The BOOT document selects the first model; the settings write moves the
  # default to the second one, which is the observable difference.
  write_settings_boot_document "$models" "http://127.0.0.1:$rec_port/v1"
  start_serve "$serve_port" "$EVIDENCE_DIR/settings-serve.log" "$models" "$password" "$tenant"

  local base="http://127.0.0.1:$serve_port" code
  serve_login "$serve_port" "$JAR" "$password"

  code="$(serve_json POST "$JAR" "$base/v1/sessions" "$prefix-session.json" '{}')"
  [ "$code" = "201" ] || die "create session returned HTTP $code: $(cat "$prefix-session.json" 2>/dev/null)"
  SESSION="$(SESSION_FILE="$prefix-session.json" python3 -c 'import json,os; d=json.load(open(os.environ["SESSION_FILE"])); print(d.get("sessionId") or d.get("id") or "")')"
  [ -n "$SESSION" ] || die "session response carries no id"

  code="$(serve_json GET "$JAR" "$base/v1/settings/models" "$prefix-get-before.json")"
  [ "$code" = "200" ] || die "GET /v1/settings/models returned HTTP $code: $(cat "$prefix-get-before.json" 2>/dev/null)"
  SETTINGS_REV_BEFORE="$(FILE="$prefix-get-before.json" python3 -c 'import json,os; print(json.load(open(os.environ["FILE"])).get("revision"))')"
  settings_turn_until "$base" "$JAR" "before" "$prefix-run-before" "设置前的一条输入" done \
    || die "the before-settings attempt never settled done; see $EVIDENCE_DIR/settings-serve.log"

  # The settings write is a whole-document replacement (not a patch over the
  # deployment document), so the body is the boot document with a new default
  # selection: a provider declared here must carry its own api/access.
  code="$(serve_json PUT "$JAR" "$base/v1/settings/models" "$prefix-put.json" \
    "$(SECOND_MODEL="${KEEL_E2E_SECOND_MODEL:-claude-sonnet-5}" EXPECTED="$SETTINGS_REV_BEFORE" BOOT="$models" \
       python3 -c '
import json, os
doc = json.load(open(os.environ["BOOT"], encoding="utf-8"))
doc["selection"] = {"providerId": "s2api-e2e", "modelId": os.environ["SECOND_MODEL"], "reasoningLevel": "low"}
doc["expectedRevision"] = int(os.environ["EXPECTED"])
print(json.dumps(doc))')")"
  SETTINGS_PUT_CODE="$code"
  info "PUT /v1/settings/models -> HTTP $code"
  [ "$code" = "200" ] || die "settings write returned HTTP $code: $(cat "$prefix-put.json" 2>/dev/null)"

  code="$(serve_json GET "$JAR" "$base/v1/settings/models" "$prefix-get-after.json")"
  SETTINGS_GET_CODE="$code"

  settings_turn_until "$base" "$JAR" "after" "$prefix-run-after" "设置后的一条输入" done \
    || die "the after-settings attempt never settled done"

  code="$(serve_json PUT "$JAR" "$base/v1/settings/models" "$prefix-put-bad.json" \
    '{"schemaVersion":1,"selection":{"providerId":"s2api-e2e","modelId":"does-not-exist"}}')"
  SETTINGS_BAD_CODE="$code"
  info "PUT with an unknown model -> HTTP $code (expected 400)"

  code="$(serve_json DELETE "$JAR" "$base/v1/settings/models?expectedRevision=$(FILE="$prefix-get-after.json" python3 -c 'import json,os; print(json.load(open(os.environ["FILE"])).get("revision"))')" "$prefix-delete.json")"
  SETTINGS_DELETE_CODE="$code"
  code="$(serve_json GET "$JAR" "$base/v1/settings/models" "$prefix-get-reset.json")"
  settings_turn_until "$base" "$JAR" "reset" "$prefix-run-reset" "重置后的一条输入" done \
    || die "the reset attempt never settled done; see $prefix-status.json"

  # Retry (change keel-workspace-polish S2): re-ask the last turn's own input.
  # The projection's derived turn id is "t_<the turn's first input seq>"; the
  # ledger grows — a NEW user.message carrying retryOf — and nothing is
  # rewritten.
  RETRY_TURN_SEQ="$(EVENTS_FILE="$prefix-run-reset-events.json" python3 -c '
import json, os
events = json.load(open(os.environ["EVENTS_FILE"])).get("events") or []
seqs = [e["seq"] for e in events if e.get("kind") == "user.message"]
print(seqs[-1] if seqs else "")')"
  [ -n "$RETRY_TURN_SEQ" ] || die "no committed user.message to retry"
  retry_until "$base" "$JAR" "$RETRY_TURN_SEQ" "$prefix-retry" 3 \
    || die "the retry never settled done; see $prefix-retry-status.json"

  SETTINGS_MODEL="$MODEL" SECOND_MODEL="${KEEL_E2E_SECOND_MODEL:-claude-sonnet-5}" \
  BEFORE="$prefix-get-before.json" AFTER="$prefix-get-after.json" RESET="$prefix-get-reset.json" \
  PUT_CODE="$SETTINGS_PUT_CODE" GET_CODE="$SETTINGS_GET_CODE" BAD_CODE="$SETTINGS_BAD_CODE" \
  PUT_BAD="$prefix-put-bad.json" \
  DELETE_CODE="$SETTINGS_DELETE_CODE" REV_BEFORE="$SETTINGS_REV_BEFORE" \
  EVENTS_BEFORE="$prefix-run-before-events.json" EVENTS_AFTER="$prefix-run-after-events.json" \
  EVENTS_RESET="$prefix-run-reset-events.json" BODIES="$bodies" \
  RETRY_SEQ="$RETRY_TURN_SEQ" RETRY_EVENTS="$prefix-retry-events.json" \
  RESET_EVENTS="$prefix-run-reset-events.json" \
  SUMMARY_OUT="$EVIDENCE_DIR/settings-summary.json" \
  python3 - <<'PY' || die "model-settings assertions failed; see $EVIDENCE_DIR/settings-summary.json"
import json, os

first, second = os.environ["SETTINGS_MODEL"], os.environ["SECOND_MODEL"]
checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "ok": bool(ok), "detail": detail})
    print(("PASS: " if ok else "FAIL: ") + name + ((" (%s)" % detail) if detail else ""), flush=True)


def load(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def finished_models(path):
    events = load(path).get("events") or []
    out = []
    for ev in events:
        if ev.get("kind") != "run.finished":
            continue
        p = ev.get("payload")
        if isinstance(p, str):
            p = json.loads(p)
        out.append(p.get("model"))
    return out


def last_finished_model(path):
    models = finished_models(path)
    return models[-1] if models else None


before, after, reset = load(os.environ["BEFORE"]), load(os.environ["AFTER"]), load(os.environ["RESET"])
check("GET before the write reports the deployment document", before.get("source") == "deployment",
      "source=%r" % before.get("source"))
check("the before run used the deployment model", last_finished_model(os.environ["EVENTS_BEFORE"]) == first,
      finished_models(os.environ["EVENTS_BEFORE"]))
check("the write was accepted", os.environ["PUT_CODE"] == "200", "HTTP %s" % os.environ["PUT_CODE"])
check("the read-back reflects the stored document",
      os.environ["GET_CODE"] == "200" and after.get("source") == "stored", "source=%r" % after.get("source"))
check("the revision moved", int(after.get("revision") or 0) == int(os.environ["REV_BEFORE"]) + 1,
      "before=%s after=%s" % (os.environ["REV_BEFORE"], after.get("revision")))
check("the stored document is the one that was written",
      ((after.get("document") or {}).get("selection") or {}).get("modelId") == second,
      (after.get("document") or {}).get("selection"))
check("the next run used the new model without a restart", last_finished_model(os.environ["EVENTS_AFTER"]) == second,
      finished_models(os.environ["EVENTS_AFTER"]))
check("a document whose selection cannot run is refused", os.environ["BAD_CODE"] == "400",
      "HTTP %s" % os.environ["BAD_CODE"])
bad = load(os.environ["PUT_BAD"])
check("the refusal names the field", bool(bad.get("issues")), bad.get("error"))
check("the reset was accepted", os.environ["DELETE_CODE"] == "200", "HTTP %s" % os.environ["DELETE_CODE"])
check("the reset restores the deployment source", reset.get("source") == "deployment",
      "source=%r" % reset.get("source"))
check("the run after the reset uses the deployment model", last_finished_model(os.environ["EVENTS_RESET"]) == first,
      finished_models(os.environ["EVENTS_RESET"]))

rows = [json.loads(line) for line in open(os.environ["BODIES"], encoding="utf-8") if line.strip()]
used = {r.get("model") for r in rows}
check("the gateway received both models", {first, second} <= used, "wire models=%s" % sorted(used))


def payload_of(event):
    p = event.get("payload")
    if isinstance(p, str):
        return json.loads(p)
    return p or {}


# Retry: the ledger grew, the original turns were not rewritten.
retry_events = load(os.environ["RETRY_EVENTS"]).get("events") or []
reset_events = load(os.environ["RESET_EVENTS"]).get("events") or []
retry_seq = int(os.environ["RETRY_SEQ"])
reasks = [e for e in retry_events if e.get("kind") == "user.message" and payload_of(e).get("retryOf") == retry_seq]
check("the retry appended one re-ask carrying retryOf", len(reasks) == 1,
      "retryOf=%s matches=%d" % (retry_seq, len(reasks)))
if reasks:
    new_seq = reasks[0]["seq"]
    later = [e for e in retry_events if e["seq"] > new_seq]
    check("the re-ask ran to a fresh terminal run",
          any(e.get("kind") == "run.finished" for e in later) and
          any(e.get("kind") == "assistant.message" for e in later),
          "seqs after %s: %s" % (new_seq, [e.get("kind") for e in later]))
    untouched = all(
        any(o["seq"] == e["seq"] and o.get("kind") == e.get("kind") for o in retry_events)
        for e in reset_events
    )
    check("the original turns are still present at their own seqs", untouched,
          "%d original events" % len(reset_events))

summary = {"ok": all(c["ok"] for c in checks), "checks": checks}
with open(os.environ["SUMMARY_OUT"], "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
print("summary: %s" % os.environ["SUMMARY_OUT"], flush=True)
raise SystemExit(0 if all(c["ok"] for c in checks) else 1)
PY

  stop_serve
  stop_record_proxy
  say ""
  say "PASS: model settings wrote, applied without a restart, and reset"
  say "  evidence   = $EVIDENCE_DIR/settings-{get-before,put,get-after,delete,get-reset}.json, settings-wire.jsonl"
}

# --- secrets phase (the credential really comes from the secret store) -----
#
# A secret-store write must be the credential the model call uses — not an env
# variable that happens to hold the same key. The proof is behavioural: writing
# a WRONG value through the API and selecting a provider that references it must
# fail authentication, and writing the right value must succeed with the same
# document. No amount of "the value is stored" assertion proves that.
run_settings_secrets() {
  info "phase: secrets (wrong value fails, right value works, value never readable)"
  local rec_port serve_port tenant password models bodies prefix
  rec_port="$(pick_port)"
  serve_port="$(pick_port)"
  prefix="$EVIDENCE_DIR/secrets"
  bodies="$EVIDENCE_DIR/secrets-wire.jsonl"
  models="$EVIDENCE_DIR/secrets-boot-models.json"
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  password="${KEEL_E2E_SERVE_PASSWORD:-keel-e2e-$$-$(date +%s)}"
  JAR="$EVIDENCE_DIR/secrets-cookies.txt"
  : > "$JAR"

  SERVE_BIN="$(ensure_serve_binary)"
  PROXY_EXPECT_KEY="$API_KEY" start_recording_proxy "$rec_port" "$EVIDENCE_DIR/secrets-recording-proxy.log" "$bodies" "$BASE_URL"
  write_settings_boot_document "$models" "http://127.0.0.1:$rec_port/v1"
  # The boot document uses the env credential; the secrets phase moves the
  # tenant to a provider that references the secret store instead.
  start_serve "$serve_port" "$EVIDENCE_DIR/secrets-serve.log" "$models" "$password" "$tenant"

  local base="http://127.0.0.1:$serve_port" code wrong
  serve_login "$serve_port" "$JAR" "$password"
  wrong="wrong-$(date +%s)-$$"

  code="$(serve_json PUT "$JAR" "$base/v1/settings/secrets/e2e_key" "$prefix-put-wrong.json" \
    "$(VALUE="$wrong" python3 -c 'import json,os; print(json.dumps({"value": os.environ["VALUE"]}))')")"
  SECRETS_PUT_CODE="$code"
  info "PUT /v1/settings/secrets/e2e_key (wrong value) -> HTTP $code"

  code="$(serve_json GET "$JAR" "$base/v1/settings/secrets" "$prefix-list.json")"
  SECRETS_LIST_CODE="$code"

  # A document that reaches the gateway through the secret reference.
  code="$(serve_json PUT "$JAR" "$base/v1/settings/models" "$prefix-put-doc.json" \
    "$(MODEL_ID="$MODEL" PROXY="http://127.0.0.1:$rec_port/v1" python3 -c '
import json, os
doc = {
  "schemaVersion": 1,
  "providers": [{
    "id": "s2api-secret", "name": "S2API (secret ref)",
    "api": {"type": "openai-chat-completions", "baseUrl": os.environ["PROXY"]},
    "access": {"type": "api-key", "ref": "secret:e2e_key"},
    "modelIds": [os.environ["MODEL_ID"]], "visibility": "visible",
  }],
  "selection": {"providerId": "s2api-secret", "modelId": os.environ["MODEL_ID"], "reasoningLevel": "low"},
}
print(json.dumps(doc))')")"
  SECRETS_DOC_CODE="$code"

  code="$(serve_json POST "$JAR" "$base/v1/sessions" "$prefix-session.json" '{}')"
  [ "$code" = "201" ] || die "create session returned HTTP $code"
  SESSION="$(SESSION_FILE="$prefix-session.json" python3 -c 'import json,os; d=json.load(open(os.environ["SESSION_FILE"])); print(d.get("sessionId") or d.get("id") or "")')"

  submit_settings_turn "$base" "$JAR" "wrong-secret" "$prefix-run-wrong" "用一句话回答：ok"
  # A wrong stored credential is expected to FAIL: the gate is that the attempt
  # ran at all and reached the gateway (401), not that it answered.
  wait_serve_run "$base" "$JAR" "$SESSION" "$prefix-events-wrong.json" "$prefix-status-wrong.json" failed \
    || die "the wrong-secret attempt did not settle failed"

  # Now the right value; the same document must start working.
  code="$(serve_json PUT "$JAR" "$base/v1/settings/secrets/e2e_key" "$prefix-put-right.json" \
    "$(VALUE="$API_KEY" python3 -c 'import json,os; print(json.dumps({"value": os.environ["VALUE"]}))')")"
  SECRETS_RIGHT_CODE="$code"
  submit_settings_turn "$base" "$JAR" "right-secret" "$prefix-run-right" "用一句话回答：ok"
  wait_serve_run "$base" "$JAR" "$SESSION" "$prefix-events-right.json" "$prefix-status-right.json" \
    || die "the right-secret attempt did not settle"

  SECRETS_PUT_CODE="$SECRETS_PUT_CODE" SECRETS_LIST_CODE="$SECRETS_LIST_CODE" \
  SECRETS_DOC_CODE="$SECRETS_DOC_CODE" SECRETS_RIGHT_CODE="$SECRETS_RIGHT_CODE" \
  LIST="$prefix-list.json" EVENTS_WRONG="$prefix-events-wrong.json" EVENTS_RIGHT="$prefix-events-right.json" \
  SECRET_NAME="e2e_key" SECRET_VALUE="$API_KEY" WRONG="$wrong" \
  SUMMARY_OUT="$EVIDENCE_DIR/secrets-summary.json" \
  python3 - <<'PY' || die "secret assertions failed; see $EVIDENCE_DIR/secrets-summary.json"
import json, os

checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "ok": bool(ok), "detail": detail})
    print(("PASS: " if ok else "FAIL: ") + name + ((" (%s)" % detail) if detail else ""), flush=True)


def load(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def payloads(path, kind):
    out = []
    for ev in load(path).get("events") or []:
        if ev.get("kind") != kind:
            continue
        p = ev.get("payload")
        if isinstance(p, str):
            p = json.loads(p)
        out.append(p)
    return out


listing = load(os.environ["LIST"])
names = [s.get("name") for s in listing.get("secrets") or []]
check("the key store is available", listing.get("available") is True, "unavailable: %s" % listing.get("reason"))
check("the write was accepted", os.environ["SECRETS_PUT_CODE"] == "200", "HTTP %s" % os.environ["SECRETS_PUT_CODE"])
check("the list shows the name", os.environ["SECRET_NAME"] in names, names)
raw = open(os.environ["LIST"], encoding="utf-8").read()
check("the list never carries the value", os.environ["SECRET_VALUE"] not in raw and os.environ["WRONG"] not in raw)
check("a document may reference the secret", os.environ["SECRETS_DOC_CODE"] == "200",
      "HTTP %s" % os.environ["SECRETS_DOC_CODE"])

wrong_assistants = payloads(os.environ["EVENTS_WRONG"], "assistant.message")
wrong_errors = [(p.get("errorMessage") or "") for p in wrong_assistants if p.get("stopReason") == "error"]
check("a wrong secret value cannot authenticate", bool(wrong_errors),
      wrong_errors[0][:120] if wrong_errors else "the attempt answered with a wrong credential")

right_assistants = payloads(os.environ["EVENTS_RIGHT"], "assistant.message")
right_ok = [p for p in right_assistants if p.get("stopReason") == "end_turn"]
check("the right secret value makes the same document work", bool(right_ok),
      [(p.get("stopReason"), (p.get("errorMessage") or "")[:80]) for p in right_assistants])

summary = {"ok": all(c["ok"] for c in checks), "checks": checks}
with open(os.environ["SUMMARY_OUT"], "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
print("summary: %s" % os.environ["SUMMARY_OUT"], flush=True)
raise SystemExit(0 if all(c["ok"] for c in checks) else 1)
PY

  stop_serve
  stop_record_proxy
  say ""
  say "PASS: the secret store supplies the credential (wrong rejected, right accepted, value unreadable)"
  say "  evidence   = $EVIDENCE_DIR/secrets-{put-wrong,list,put-doc,run-wrong,put-right,run-right}.json"
}

# write_settings_boot_document FILE PROXY_BASE — the deployment document used by
# the settings phase: one provider with two models, defaulting to the first.
write_settings_boot_document() {
  local file="$1" base="$2"
  MODELS_FILE="$file" PROXY_BASE="$base" MODEL_ID="$MODEL" SECOND_MODEL="${KEEL_E2E_SECOND_MODEL:-claude-sonnet-5}" python3 - <<'PY'
import json, os

doc = {
    "schemaVersion": 1,
    "providers": [{
        "id": "s2api-e2e",
        "name": "S2API (recording proxy)",
        "api": {"type": "openai-chat-completions", "baseUrl": os.environ["PROXY_BASE"]},
        "access": {"type": "api-key", "env": "KEEL_E2E_MODEL_KEY"},
        "modelIds": [os.environ["MODEL_ID"], os.environ["SECOND_MODEL"]],
        "visibility": "visible",
    }],
    "selection": {"providerId": "s2api-e2e", "modelId": os.environ["MODEL_ID"], "reasoningLevel": "low"},
}
with open(os.environ["MODELS_FILE"], "w", encoding="utf-8") as fh:
    json.dump(doc, fh, ensure_ascii=False, indent=2)
PY
  info "settings boot document written: $file (default $MODEL, second ${KEEL_E2E_SECOND_MODEL:-claude-sonnet-5})"
}

# settings_turn_until BASE JAR LABEL PREFIX TEXT EXPECTED [ATTEMPTS] — submit
# one input and wait for the expected terminal status. The gateway has stalled
# or answered HTML for minutes at a time during this work; the deployment's own
# recovery is a fresh submit, so the harness does the same instead of failing the
# gate on provider weather. A run that never settles is NOT retried into
# obscurity: the attempts are bounded and the last status is reported.
settings_turn_until() {
  local base="$1" jar="$2" label="$3" prefix="$4" text="$5" expected="$6" attempts="${7:-3}"
  local i status
  for i in $(seq 1 "$attempts"); do
    submit_settings_turn "$base" "$jar" "$label-$i" "$prefix" "$text"
    if wait_serve_run "$base" "$jar" "$SESSION" "$prefix-events.json" "$prefix-status.json" "$expected"; then
      return 0
    fi
    status="$(RUN_FILE="$prefix-status.json" python3 -c 'import json,os
try: print(json.load(open(os.environ["RUN_FILE"])).get("status") or "none")
except Exception: print("none")')"
    info "$label attempt $i did not settle $expected (status=$status); resubmitting"
  done
  return 1
}

# submit_settings_turn BASE JAR LABEL PREFIX TEXT — one plain input (no
# selection: the tenant default must supply the identity).
submit_settings_turn() {
  local base="$1" jar="$2" label="$3" prefix="$4" text="$5" code
  code="$(serve_json POST "$jar" "$base/v1/sessions/$SESSION/input" "$prefix-input.json" \
    "$(REQUEST_ID="settings-$label-$(date +%s)-$$" TEXT="$text" python3 -c '
import json, os
print(json.dumps({"text": os.environ["TEXT"], "requestId": os.environ["REQUEST_ID"]}))')")"
  [ "$code" = "201" ] || die "input ($label) returned HTTP $code: $(cat "$prefix-input.json" 2>/dev/null)"
  info "input committed ($label)"
}

# retry_until BASE JAR TURN_SEQ PREFIX [ATTEMPTS] — POST /v1/sessions/{id}/retry
# until the re-asked turn settles. A retry is intentionally not idempotent (a
# retry that deduplicated against the original would be a no-op), so provider
# weather is answered with another intentional attempt, bounded like the
# settings turns.
retry_until() {
  local base="$1" jar="$2" seq="$3" prefix="$4" attempts="${5:-3}" i code status
  for i in $(seq 1 "$attempts"); do
    code="$(serve_json POST "$jar" "$base/v1/sessions/$SESSION/retry" "$prefix-attempt-$i.json" \
      "{\"turnId\":\"t_$seq\"}")"
    if [ "$code" != "201" ]; then
      info "retry attempt $i returned HTTP $code: $(cat "$prefix-attempt-$i.json" 2>/dev/null)"
      sleep 2
      continue
    fi
    if wait_serve_run "$base" "$jar" "$SESSION" "$prefix-events.json" "$prefix-status.json" done; then
      return 0
    fi
    status="$(RUN_FILE="$prefix-status.json" python3 -c 'import json,os
try: print(json.load(open(os.environ["RUN_FILE"])).get("status") or "none")
except Exception: print("none")')"
    info "retry attempt $i did not settle done (status=$status); retrying"
    sleep 2
  done
  return 1
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

# --- current binary on the flat configuration (compatibility gate) ---------
#
# The other half of the compatibility promise: this branch's binary, started
# with ONLY the flat flags, must send exactly what the baseline sent (same wire
# model, same thinking level, no request overlay) AND must refuse a selection
# instead of silently ignoring one — a deployment with no provider document has
# no vocabulary to vet it, so storing it would be a promise nobody keeps.
run_current_legacy() {
  info "phase: current binary on the flat configuration (compatibility)"
  local rec_port serve_port tenant password bodies code
  rec_port="$(pick_port)"
  serve_port="$(pick_port)"
  bodies="$EVIDENCE_DIR/legacy-wire-requests.jsonl"
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  password="${KEEL_E2E_SERVE_PASSWORD:-keel-e2e-$$-$(date +%s)}"
  JAR="$EVIDENCE_DIR/legacy-cookies.txt"
  : > "$JAR"

  SERVE_BIN="$(ensure_serve_binary)"
  PROXY_EXPECT_KEY="$API_KEY" start_recording_proxy "$rec_port" "$EVIDENCE_DIR/legacy-recording-proxy.log" "$bodies" "$BASE_URL"
  KEEL_MODELS_CONFIG="" \
  SERVE_BASE_URL="http://127.0.0.1:$rec_port/v1" \
  SERVE_MODEL="$MODEL" SERVE_PROTOCOL=openai SERVE_KEY="$API_KEY" SERVE_TENANT="$tenant" \
  start_serve_legacy "$serve_port" "$EVIDENCE_DIR/legacy-serve.log" "$password"

  local base="http://127.0.0.1:$serve_port"
  serve_login "$serve_port" "$JAR" "$password"

  code="$(serve_json GET "$JAR" "$base/v1/providers" "$EVIDENCE_DIR/legacy-providers.json")"
  LEGACY_PROVIDERS_CODE="$code"
  info "current binary, no models document: GET /v1/providers -> HTTP $code (expected 501)"

  code="$(serve_json POST "$JAR" "$base/v1/sessions" "$EVIDENCE_DIR/legacy-session.json" '{}')"
  [ "$code" = "201" ] || die "create session returned HTTP $code: $(cat "$EVIDENCE_DIR/legacy-session.json" 2>/dev/null)"
  SESSION="$(SESSION_FILE="$EVIDENCE_DIR/legacy-session.json" python3 -c 'import json,os; d=json.load(open(os.environ["SESSION_FILE"])); print(d.get("sessionId") or d.get("id") or "")')"

  # 1. A selection is refused: 400, nothing committed.
  code="$(serve_json POST "$JAR" "$base/v1/sessions/$SESSION/input" "$EVIDENCE_DIR/legacy-refused.json" \
    "$(REQUEST_ID="legacy-refused-$(date +%s)-$$" MODEL_ID="$MODEL" python3 -c '
import json, os
print(json.dumps({"text": "this must not be stored",
                  "requestId": os.environ["REQUEST_ID"],
                  "selection": {"providerId": "s2api-e2e", "modelId": os.environ["MODEL_ID"], "reasoningLevel": "high"}}))'  )")"
  LEGACY_REFUSED_CODE="$code"

  # 2. A plain input runs exactly like the baseline did.
  code="$(serve_json POST "$JAR" "$base/v1/sessions/$SESSION/input" "$EVIDENCE_DIR/legacy-input.json" \
    "$(REQUEST_ID="legacy-$(date +%s)-$$" python3 -c '
import json, os
print(json.dumps({"text": "Call the echo tool exactly once with the argument x set to \"legacy-marker\". Then reply with one short sentence that contains the exact text the tool returned.",
                  "requestId": os.environ["REQUEST_ID"]}))'  )")"
  [ "$code" = "201" ] || die "input returned HTTP $code: $(cat "$EVIDENCE_DIR/legacy-input.json" 2>/dev/null)"
  wait_serve_run "$base" "$JAR" "$SESSION" "$EVIDENCE_DIR/legacy-events.json" "$EVIDENCE_DIR/legacy-run.json" \
    || die "the flat-configuration attempt did not settle done; see $EVIDENCE_DIR/legacy-serve.log"

  MODEL_ID="$MODEL" BODIES="$bodies" REFUSED_CODE="$LEGACY_REFUSED_CODE" \
  PROVIDERS_CODE="$LEGACY_PROVIDERS_CODE" EVENTS="$EVIDENCE_DIR/legacy-events.json" \
  BASELINE_BODIES="$EVIDENCE_DIR/baseline-wire-requests.jsonl" \
  SUMMARY_OUT="$EVIDENCE_DIR/legacy-summary.json" \
  python3 - <<'PY' || die "compatibility assertions failed; see $EVIDENCE_DIR/legacy-summary.json"
import json, os

model = os.environ["MODEL_ID"]
checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "ok": bool(ok), "detail": detail})
    print(("PASS: " if ok else "FAIL: ") + name + ((" (%s)" % detail) if detail else ""), flush=True)


def rows(path):
    if not os.path.exists(path):
        return []
    out = []
    for line in open(path, encoding="utf-8"):
        if line.strip():
            row = json.loads(line)
            if row.get("path", "").endswith("/chat/completions"):
                out.append(row)
    return out


current = rows(os.environ["BODIES"])
baseline = rows(os.environ["BASELINE_BODIES"])
check("the selection was refused before anything was committed",
      os.environ["REFUSED_CODE"] == "400", "HTTP %s" % os.environ["REFUSED_CODE"])
check("no provider document means no settings surface",
      os.environ["PROVIDERS_CODE"] == "501", "HTTP %s" % os.environ["PROVIDERS_CODE"])
check("the flat configuration still reaches the gateway", bool(current), "%d requests" % len(current))
shape = lambda rows_: sorted({(r.get("model"), r.get("reasoningEffort"), r.get("maxTokens")) for r in rows_})
check("current wire shape equals the baseline wire shape", bool(current) and shape(current) == shape(baseline),
      "current=%s baseline=%s" % (shape(current), shape(baseline)))
events = json.load(open(os.environ["EVENTS"], encoding="utf-8")).get("events") or []
refused = [ev for ev in events if ev.get("kind") == "user.message"
           and "must not be stored" in json.dumps(ev.get("payload"))]
check("the refused input is not in the log", not refused, "%d matching events" % len(refused))

summary = {"ok": all(c["ok"] for c in checks), "model": model, "requests": current, "checks": checks}
with open(os.environ["SUMMARY_OUT"], "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
print("summary: %s" % os.environ["SUMMARY_OUT"], flush=True)
raise SystemExit(0 if all(c["ok"] for c in checks) else 1)
PY

  stop_serve
  stop_record_proxy
  say ""
  say "PASS: the flat configuration is unchanged and a selection is refused without a document"
  say "  evidence   = $EVIDENCE_DIR/legacy-wire-requests.jsonl, legacy-summary.json"
}

# --- baseline (before) phase -----------------------------------------------
#
# The "before" half of the provider-configuration comparison, and the
# legacy-configuration compatibility gate: the pre-change binary, configured
# with the flat flags, must ignore a selection (it has no provider document),
# which is exactly what the after phase proves it now honours. Both halves write
# the same evidence shape, so a report can diff them field by field.
ensure_baseline_binary() {
  local ref="${KEEL_E2E_BASELINE_REF:-ed3f733}"
  local bin="${KEEL_E2E_BASELINE_BIN:-/tmp/keel-real-model-baseline-$ref}"
  if [ -x "$bin" ]; then
    printf '%s\n' "$bin"
    return 0
  fi
  local src="/tmp/keel-baseline-src-$ref"
  rm -rf "$src"
  mkdir -p "$src"
  info "extracting baseline $ref -> $src"
  git archive "$ref" | tar -x -C "$src" || die "git archive $ref failed (is the ref present?)"
  ( cd "$src" && go build -o "$bin" ./cmd/keel ) || die "go build baseline ./cmd/keel failed"
  printf '%s\n' "$bin"
}

run_baseline() { # REF
  local ref="$1"
  info "phase: baseline $ref (flat configuration, selection must be ignored)"
  local rec_port serve_port tenant password bodies models
  rec_port="$(pick_port)"
  serve_port="$(pick_port)"
  bodies="$EVIDENCE_DIR/baseline-wire-requests.jsonl"
  models="$EVIDENCE_DIR/baseline-models-config.json"
  tenant="$(uuidgen | tr '[:upper:]' '[:lower:]')"
  password="${KEEL_E2E_SERVE_PASSWORD:-keel-e2e-$$-$(date +%s)}"
  JAR="$EVIDENCE_DIR/baseline-cookies.txt"
  : > "$JAR"

  SERVE_BIN="$(ensure_baseline_binary)"
  PROXY_EXPECT_KEY="$API_KEY" start_recording_proxy "$rec_port" "$EVIDENCE_DIR/baseline-recording-proxy.log" "$bodies" "$BASE_URL"
  # A models document IS written and exported, so the run proves the baseline
  # ignores it (the feature did not exist) rather than proving the flag is
  # unset.
  write_models_config "$models" "http://127.0.0.1:$rec_port/v1" "$MODEL"

  local base="http://127.0.0.1:$serve_port" code
  # Flat flags only: the baseline binary has no --models-config at all.
  KEEL_MODELS_CONFIG="$models" \
  SERVE_BASE_URL="http://127.0.0.1:$rec_port/v1" \
  SERVE_MODEL="$MODEL" SERVE_PROTOCOL=openai SERVE_KEY="$API_KEY" SERVE_TENANT="$tenant" \
  start_serve_legacy "$serve_port" "$EVIDENCE_DIR/baseline-serve.log" "$password"
  base="http://127.0.0.1:$serve_port"
  serve_login "$serve_port" "$JAR" "$password"

  code="$(serve_json GET "$JAR" "$base/v1/providers" "$EVIDENCE_DIR/baseline-providers.json")"
  BASELINE_PROVIDERS_CODE="$code"
  info "baseline GET /v1/providers -> HTTP $code (expected: absent route)"

  code="$(serve_json POST "$JAR" "$base/v1/sessions" "$EVIDENCE_DIR/baseline-session.json" '{}')"
  [ "$code" = "201" ] || die "baseline create session returned HTTP $code: $(cat "$EVIDENCE_DIR/baseline-session.json" 2>/dev/null)"
  SESSION="$(SESSION_FILE="$EVIDENCE_DIR/baseline-session.json" python3 -c 'import json,os; d=json.load(open(os.environ["SESSION_FILE"])); print(d.get("sessionId") or d.get("id") or "")')"

  local prompt
  prompt="Call the echo tool exactly once with the argument x set to \"baseline-marker\". Then reply with one short sentence that contains the exact text the tool returned."
  code="$(serve_json POST "$JAR" "$base/v1/sessions/$SESSION/input" "$EVIDENCE_DIR/baseline-input.json" \
    "$(REQUEST_ID="baseline-$(date +%s)-$$" PROMPT="$prompt" MODEL_ID="$MODEL" python3 -c '
import json, os
print(json.dumps({
    "text": os.environ["PROMPT"],
    "requestId": os.environ["REQUEST_ID"],
    "selection": {"providerId": "s2api-e2e", "modelId": os.environ["MODEL_ID"], "reasoningLevel": "high"},
}))'  )")"
  [ "$code" = "201" ] || die "baseline input returned HTTP $code: $(cat "$EVIDENCE_DIR/baseline-input.json" 2>/dev/null)"
  wait_serve_run "$base" "$JAR" "$SESSION" "$EVIDENCE_DIR/baseline-events.json" "$EVIDENCE_DIR/baseline-run.json" \
    || die "baseline attempt did not settle done; see $EVIDENCE_DIR/baseline-serve.log"

  MODEL_ID="$MODEL" BODIES="$bodies" PROVIDERS_CODE="$BASELINE_PROVIDERS_CODE" \
  EVENTS="$EVIDENCE_DIR/baseline-events.json" REF="$ref" \
  SUMMARY_OUT="$EVIDENCE_DIR/baseline-summary.json" \
  python3 - <<'PY' || die "baseline assertions failed; see $EVIDENCE_DIR/baseline-summary.json"
import json, os

model = os.environ["MODEL_ID"]
bodies = [json.loads(line) for line in open(os.environ["BODIES"], encoding="utf-8") if line.strip()]
events = json.load(open(os.environ["EVENTS"], encoding="utf-8")).get("events") or []
checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "ok": bool(ok), "detail": detail})
    print(("PASS: " if ok else "FAIL: ") + name + ((" (%s)" % detail) if detail else ""), flush=True)


def payload(ev):
    raw = ev.get("payload")
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except Exception:
            raw = {}
    return raw if isinstance(raw, dict) else {}


chat = [b for b in bodies if b.get("path", "").endswith("/chat/completions")]
check("baseline called the gateway through the flat configuration", bool(chat), "requests=%d" % len(chat))
check("baseline had no provider-settings route", os.environ["PROVIDERS_CODE"] in ("404", "501", "405"),
      "HTTP %s" % os.environ["PROVIDERS_CODE"])
check("baseline used the legacy wire model", bool(chat) and all(b.get("model") == model for b in chat),
      "models=%s" % sorted({b.get("model") for b in chat}))
check("baseline ignored the selection's reasoning level (flat default low)",
      bool(chat) and all(b.get("reasoningEffort") == "low" for b in chat),
      "efforts=%s" % sorted({b.get("reasoningEffort") for b in chat}))
check("baseline applied no request overlay (no configured max tokens)",
      bool(chat) and all(b.get("maxTokens") is None for b in chat),
      "maxTokens=%s" % [b.get("maxTokens") for b in chat])
users = [payload(ev) for ev in events if ev.get("kind") == "user.message"]
check("baseline stored no selection fact (the field did not exist, so the value was dropped at the edge)",
      bool(users) and all(u.get("selection") is None for u in users),
      "selections=%s" % [u.get("selection") for u in users])
finished = [payload(ev) for ev in events if ev.get("kind") == "run.finished"]
check("baseline run summary carries no execution identity",
      not any(f.get("model") or f.get("provider") for f in finished),
      "identities=%s" % [(f.get("provider"), f.get("model")) for f in finished])

summary = {"ok": all(c["ok"] for c in checks), "ref": os.environ["REF"], "requests": bodies, "checks": checks}
with open(os.environ["SUMMARY_OUT"], "w", encoding="utf-8") as fh:
    json.dump(summary, fh, ensure_ascii=False, indent=2)
print("summary: %s" % os.environ["SUMMARY_OUT"], flush=True)
raise SystemExit(0 if all(c["ok"] for c in checks) else 1)
PY

  stop_serve
  stop_record_proxy
  say ""
  say "PASS: baseline $ref (flat configuration, selection ignored)"
  say "  evidence   = $EVIDENCE_DIR/baseline-wire-requests.jsonl, baseline-summary.json"
}

# start_serve_legacy PORT LOG PASSWORD — the flat-configuration boot, used by
# the baseline phase (a pre-provider binary has no models document flag).
start_serve_legacy() {
  local port="$1" log="$2" password="$3"
  SERVE_LOG="$log"
  : > "$log"
  info "starting baseline keel serve on 127.0.0.1:$port (log: $log)"
  env KEEL_DSN="$DSN" "$SERVE_BIN" serve -addr "127.0.0.1:$port" -cas "$EVIDENCE_DIR/baseline-cas" \
    -password "$password" -tenant "${SERVE_TENANT:?}" \
    -model "${SERVE_MODEL:?}" -base-url "${SERVE_BASE_URL:?}" -protocol "${SERVE_PROTOCOL:?}" -api-key "${SERVE_KEY:?}" \
    >>"$log" 2>&1 &
  SERVE_PID=$!

  local body pid i
  for i in $(seq 1 150); do
    body="$(curl -fsS -m 2 "http://127.0.0.1:$port/v1/health" 2>/dev/null || true)"
    if [ -n "$body" ]; then
      pid="$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid"))' 2>/dev/null || true)"
      [ "$pid" = "$SERVE_PID" ] || die "port $port answers with pid $pid, not $SERVE_PID (foreign process)"
      info "baseline keel serve healthy (pid $SERVE_PID)"
      return 0
    fi
    kill -0 "$SERVE_PID" 2>/dev/null || die "baseline keel serve exited during startup; see $log"
    sleep 0.2
  done
  die "baseline keel serve did not become healthy within 30s; see $log"
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
  models)  run_provider_config ;;
  models-baseline) run_baseline "${KEEL_E2E_BASELINE_REF:-ed3f733}" ;;
  models-legacy) run_current_legacy ;;
  models-all) run_baseline "${KEEL_E2E_BASELINE_REF:-ed3f733}"; run_current_legacy; run_provider_config ;;
  settings) run_model_settings; run_settings_secrets ;;
  *) die "internal: unknown mode $MODE" ;;
esac

say ""
say "evidence: $EVIDENCE_DIR (raw logs/events; not committed)"
