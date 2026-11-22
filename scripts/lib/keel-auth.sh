#!/usr/bin/env bash
# Shared account provisioning/login for shell E2E scripts.
#
# A fresh test database has no super admin. `keel admin bootstrap` mints a
# one-time setup token; the browser/CLI then completes setup with a username and
# password. This helper does that once per throwaway database and leaves:
#
#   KEEL_E2E_USERNAME / KEEL_ACCOUNT_USERNAME
#   KEEL_E2E_PASSWORD / KEEL_ACCOUNT_PASSWORD  (final password, not printed)
#   KEEL_E2E_ACCOUNT_ID
#   KEEL_E2E_JAR                               (curl -b / -c cookie jar)
#   KEEL_E2E_CSRF                              (X-Keel-CSRF for writes)
#
# It never prints a password or a token. Scripts that only need a loopback
# feature fixture may instead start `keel serve --auth local-only --tenant …`
# and skip login entirely; lanes that exercise the browser gate use this.
#
# Usage (after the server reports healthy):
#   source "$ROOT/scripts/lib/keel-auth.sh"
#   keel_auth_provision "http://127.0.0.1:$PORT" "$KEEL_BIN" "$TEST_DSN" "$TENANT" "$RUN_DIR"

KEEL_AUTH_LIB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

keel_auth_provision() { # BASE BIN DSN TENANT DIR
  local base="$1" bin="$2" dsn="$3" tenant="$4" dir="$5"
  local user="e2e-admin"
  local token_file="$dir/setup.token"
  local initial final
  initial="$(openssl rand -hex 16)"
  final="$(openssl rand -hex 16)"

  # --token-file is the only copy that survives; both streams are discarded so
  # a failure path cannot print the one-time token into a log.
  if ! KEEL_DSN="$dsn" KEEL_SERVE_TENANT="$tenant" \
    "$bin" admin bootstrap --tenant "$tenant" --token-file "$token_file" >/dev/null 2>&1; then
    echo "keel-auth: keel admin bootstrap failed (token file: $token_file)" >&2
    return 1
  fi

  KEEL_ACCOUNT_PASSWORD="$initial" KEEL_ACCOUNT_NEW_PASSWORD="$final" \
    python3 "$KEEL_AUTH_LIB_ROOT/scripts/lib/keel-auth.py" bootstrap \
      --base "$base" --token-file "$token_file" --username "$user" \
      --jar "$dir/cookies.txt" --csrf-file "$dir/csrf.txt" > "$dir/auth.json"
  rm -f "$token_file"

  KEEL_E2E_USERNAME="$user"
  KEEL_E2E_PASSWORD="$final"
  KEEL_E2E_JAR="$dir/cookies.txt"
  KEEL_E2E_CSRF="$(cat "$dir/csrf.txt")"
  KEEL_E2E_ACCOUNT_ID="$(python3 - "$dir/auth.json" <<'PY'
import json, sys
print(json.load(open(sys.argv[1])).get("accountId") or "")
PY
)"

  # The product CLI reads these; the shell E2E never passes a password flag.
  export KEEL_ACCOUNT_USERNAME="$KEEL_E2E_USERNAME"
  export KEEL_ACCOUNT_PASSWORD="$KEEL_E2E_PASSWORD"
  export KEEL_E2E_USERNAME KEEL_E2E_PASSWORD KEEL_E2E_JAR KEEL_E2E_CSRF KEEL_E2E_ACCOUNT_ID
}

keel_auth_cleanup() { # DIR
  local dir="$1"
  rm -f "$dir/cookies.txt" "$dir/csrf.txt" "$dir/auth.json" "$dir/setup.token" 2>/dev/null || true
}
