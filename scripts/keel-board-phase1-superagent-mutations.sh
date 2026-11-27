#!/usr/bin/env bash
# Mutation gate for the superagent-board lane (keel-board-v1 11.x review).
#
# Each mutation must turn its targeted test red. A surviving mutation means the
# test does not actually pin the reviewed property. The script restores every
# file it touches, even when a test fails.
#
# Usage: scripts/keel-board-phase1-superagent-mutations.sh
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"
backup="$(mktemp -d)"
restore_files() {
  if [ -f "$backup/token.go" ]; then cp "$backup/token.go" internal/superagentboard/token.go; fi
  if [ -f "$backup/bridge.go" ]; then cp "$backup/bridge.go" internal/superagentboard/bridge.go; fi
  if [ -f "$backup/commands.go" ]; then cp "$backup/commands.go" internal/superagentboard/commands.go; fi
}
cleanup() {
  restore_files
  rm -rf "$backup"
}
trap cleanup EXIT

cp internal/superagentboard/token.go "$backup/token.go"
cp internal/superagentboard/bridge.go "$backup/bridge.go"
cp internal/superagentboard/commands.go "$backup/commands.go"

failures=0
mutate() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
if old not in text:
    raise SystemExit(f"mutation anchor not found in {path}: {old!r}")
open(path, "w").write(text.replace(old, new, 1))
PY
}
expect_red() {
  local name="$1" pattern="$2"
  echo "== mutation: $name"
  if go test ./internal/superagentboard/ -run "$pattern" -count=1 >/tmp/keel-mutation.log 2>&1; then
    echo "SURVIVED: $name"
    cat /tmp/keel-mutation.log
    failures=$((failures + 1))
  else
    echo "RED: $name"
  fi
}

# 1. The deterministic token must depend on tenant/assignment/session.
mutate internal/superagentboard/token.go \
  'message := "board-claim:v1:" + tenantID.String() + ":" + assignmentID.String() + ":" + sessionID.String()' \
  'message := "board-claim:v1:constant"'
expect_red "claim token ignores its scope" 'TestClaimTokenIsDeterministicAndBounded'
restore_files

# 2. claim-ready must reject ancestor/descendant overlap, not only equality.
mutate internal/superagentboard/bridge.go \
  'return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")' \
  'return a == b'
expect_red "claim-ready only checks equality" 'TestBridgeClaimReadyOverlapGuard'
restore_files

# 3. A board block must ask an answerable question.
mutate internal/superagentboard/commands.go 'AllowFreeText: true,' 'AllowFreeText: false,'
expect_red "block asks an unanswerable question" 'TestBridgeBlockSendsAnswerableAsk'
restore_files

# 4. Every bridge action must pass the supervisor task.write gate.
mutate internal/superagentboard/bridge.go \
  'if gateErr := b.Operator.AuthorizeTaskWrite(r.Context(), tenantID); gateErr != nil {' \
  'if gateErr := error(nil); gateErr != nil {'
expect_red "bridge skips the task.write gate" 'TestBridgeRequiresSupervisorTaskWrite'
restore_files

if [ "$failures" -ne 0 ]; then
  echo "mutation gate: $failures survived" >&2
  exit 1
fi
echo "mutation gate: 4/4 red"
