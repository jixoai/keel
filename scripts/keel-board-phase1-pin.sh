#!/usr/bin/env bash
# Writes the Phase 1 dependency/evidence pin from repository facts. The board
# commit is the one NOTICE records for the tagged module; the keel commit is
# this checkout's HEAD. Run after the evidence gates have been regenerated.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
out="$repo_root/openspec/changes/keel-board-v1/evidence/phase-1-pin.txt"
cd "$repo_root"

keel_commit="$(git rev-parse HEAD)"
module="$(go list -m -f '{{.Path}} {{.Version}}' github.com/jixoai/keel-board)"
board_commit="$(sed -n 's/.*commit \([0-9a-f]\{40\}\).*/\1/p' NOTICE | head -1)"
if [ -z "$board_commit" ]; then
  echo "keel-board-phase1-pin: NOTICE has no board commit" >&2
  exit 1
fi
generated="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

cat >"$out" <<PIN
# Phase 1 evidence pin (keel-board-v1)

- keel commit: $keel_commit
- board module: $module
- board commit: $board_commit (tag v0.1.1, recorded in NOTICE)
- generated: $generated
- budget: 4096 KiB per binary and target (keel; cmd/runtime adds no dependency)
- binary-size base: 6ee7fe3

Gates (run on the keel commit above, KEEL_TEST_DSN pointing at a dedicated test database):

    KEEL_BOARD_BASE=6ee7fe3 scripts/keel-board-phase1-gate.sh
    KEEL_BOARD_BASE=6ee7fe3 scripts/keel-board-phase1-sync-gate.sh
    KEEL_BOARD_BASE=6ee7fe3 scripts/keel-board-phase1-superagent-gate.sh
    go test -race ./internal/boardintegration ./internal/superagentboard ./internal/store ./cmd/keel -count=1
    go test -p 1 ./... -count=1
    go test github.com/jixoai/keel-board/conformance
    go run ./cmd/board-sync-e2e -dsn "\$KEEL_TEST_DSN" -out openspec/changes/keel-board-v1/evidence/phase-1-sync-e2e.html
    (cd web && nsh check)
    openspec validate keel-board-v1 --strict
PIN

echo "wrote $out"
